package main

import (
	"flag"
	"log"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"time"

	influxdb "github.com/influxdb/influxdb/client"
	collectd "github.com/paulhammond/gocollectd"
	"github.com/samalba/dockerclient"
)

const appName = "influxdb-collectd-proxy"
const influxWriteInterval = time.Second
const influxWriteLimit = 50
const packetChannelSize = 100

var (
	proxyPort   *string
	typesdbPath *string
	logPath     *string
	verbose     *bool
	https       *bool

	// influxdb options
	host      *string
	username  *string
	password  *string
	database  *string
	normalize *bool

	docker *dockerT

	types       Types
	client      *influxdb.Client
	beforeCache map[string]CacheEntry
)

type dockerT struct {
	sync.Mutex
	client *dockerclient.DockerClient
	names  map[string]string
}

// point cache to perform data normalization for COUNTER and DERIVE types
type CacheEntry struct {
	Timestamp int64
	Value     float64
}

// signal handler
func handleSignals(c chan os.Signal) {
	// block until a signal is received
	sig := <-c

	log.Printf("exit with a signal: %v\n", sig)
	os.Exit(1)
}

func init() {
	log.SetPrefix("[" + appName + "] ")

	// proxy options
	proxyPort = flag.String("proxyport", "8096", "port for proxy")
	typesdbPath = flag.String("typesdb", "types.db", "path to Collectd's types.db")
	logPath = flag.String("logfile", "proxy.log", "path to log file")
	verbose = flag.Bool("verbose", false, "true if you need to trace the requests")

	// influxdb options
	host = flag.String("influxdb", "localhost:8086", "host:port for influxdb")
	username = flag.String("username", "root", "username for influxdb")
	password = flag.String("password", "root", "password for influxdb")
	database = flag.String("database", "", "database for influxdb")
	normalize = flag.Bool("normalize", true, "true if you need to normalize data for COUNTER and DERIVE types (over time)")
	https = flag.Bool("https", false, "true if you want the influxdb client to connect over https")

	// docker options
	dockerSock := flag.String("docker", "", "Docker socket e.g. unix:///var/run/docker.sock")

	flag.Parse()

	var err error

	if *dockerSock != "" {
		docker = &dockerT{}
		// Init the client
		dc, err := dockerclient.NewDockerClient(*dockerSock, nil)
		if err != nil {
			log.Fatal(err)
		}
		docker.client = dc
		go func() {
			for {
				if err := docker.updateNames(); err != nil {
					log.Printf("Error updating Docker container names, err '%s'\n", err)
				}
				time.Sleep(1 * time.Minute)
			}
		}()
	}

	beforeCache = make(map[string]CacheEntry)

	// read types.db
	types, err = ParseTypesDB(*typesdbPath)
	if err != nil {
		log.Fatalf("failed to read types.db: %v\n", err)
	}
}

func main() {
	logFile, err := os.OpenFile(*logPath, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("failed to open file: %v\n", err)
	}
	log.SetOutput(logFile)
	defer logFile.Close()

	// make influxdb client
	client, err = influxdb.NewClient(&influxdb.ClientConfig{
		Host:     *host,
		Username: *username,
		Password: *password,
		Database: *database,
		IsSecure: *https,
	})
	if err != nil {
		log.Fatalf("failed to make a influxdb client: %v\n", err)
	}

	// register a signal handler
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, os.Interrupt, os.Kill)
	go handleSignals(sc)

	// make channel for collectd
	c := make(chan collectd.Packet, packetChannelSize)

	// then start to listen
	go collectd.Listen("0.0.0.0:"+*proxyPort, c)
	log.Printf("proxy started on %s\n", *proxyPort)
	timer := time.Now()
	seriesGroup := make([]*influxdb.Series, 0)
	for packet := range c {
		seriesGroup = append(seriesGroup, processPacket(packet)...)

		if time.Since(timer) < influxWriteInterval && len(seriesGroup) < influxWriteLimit {
			continue
		} else {
			if len(seriesGroup) > 0 {
				go backendWriter(seriesGroup)
				seriesGroup = make([]*influxdb.Series, 0)
			}
			timer = time.Now()
		}
	}
}

func backendWriter(seriesGroup []*influxdb.Series) {
	if err := client.WriteSeries(seriesGroup); err != nil {
		log.Printf("failed to write series group to influxdb: %s\n", err)
	}
	if *verbose {
		log.Printf("[TRACE] wrote %d series\n", len(seriesGroup))
	}
}

func processPacket(packet collectd.Packet) []*influxdb.Series {
	if *verbose {
		log.Printf("[TRACE] got a packet: %v\n", packet)
	}

	var seriesGroup []*influxdb.Series
	// for all metrics in the packet
	for i, _ := range packet.ValueNames() {
		values, _ := packet.ValueNumbers()

		// get a type for this packet
		t := types[packet.Type]

		// pass the unknowns
		if t == nil && packet.TypeInstance == "" {
			log.Printf("unknown type instance on %s\n", packet.Plugin)
			continue
		}

		// as hostname contains commas, let's replace them
		hostName := strings.Replace(packet.Hostname, ".", "_", -1)

		// Try and resolve Docker container ID to real hostname
		if docker != nil {
			docker.Lock()
			if realName, ok := docker.names[hostName]; ok {
				hostName = realName
			}
			docker.Unlock()
		}

		// if there's a PluginInstance, use it
		pluginName := packet.Plugin
		if packet.PluginInstance != "" {
			pluginName += "-" + packet.PluginInstance
		}

		// if there's a TypeInstance, use it
		typeName := packet.Type
		if packet.TypeInstance != "" {
			typeName += "-" + packet.TypeInstance
		} else if t != nil {
			typeName += "-" + t[i]
		}

		cacheKey := hostName + "." + pluginName + "." + typeName
		name := pluginName + "." + typeName

		// influxdb stuffs
		timestamp := packet.Time().UnixNano() / 1000000
		value := values[i].Float64()
		dataType := packet.DataTypes[i]
		readyToSend := true
		normalizedValue := value

		if *normalize && dataType == collectd.TypeCounter || dataType == collectd.TypeDerive {
			if before, ok := beforeCache[cacheKey]; ok && !math.IsNaN(before.Value) {
				// normalize over time
				if timestamp-before.Timestamp > 0 {
					normalizedValue = (value - before.Value) / float64((timestamp-before.Timestamp)/1000)
				} else {
					normalizedValue = value - before.Value
				}
			} else {
				// skip current data if there's no initial entry
				readyToSend = false
			}
			entry := CacheEntry{
				Timestamp: timestamp,
				Value:     value,
			}
			beforeCache[cacheKey] = entry
		}

		if readyToSend {
			series := &influxdb.Series{
				Name:    name,
				Columns: []string{"time", "value", "host"},
				Points: [][]interface{}{
					[]interface{}{timestamp, normalizedValue, hostName},
				},
			}
			if *verbose {
				log.Printf("[TRACE] ready to send series: %v\n", series)
			}
			seriesGroup = append(seriesGroup, series)
		}
	}
	return seriesGroup
}

func (d *dockerT) updateNames() error {
	containers, err := d.client.ListContainers(true, true, "")
	if err != nil {
		return err
	}
	d.Lock()
	defer d.Unlock()
	d.names = make(map[string]string)
	for _, c := range containers {
		info, err := d.client.InspectContainer(c.Id)
		if err != nil {
			return err
		}

		if info.State.Running {
			d.names[c.Id] = strings.TrimPrefix(c.Names[0], "/")
		}
	}

	return nil
}
