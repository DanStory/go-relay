package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"

	log "github.com/Sirupsen/logrus"
	"github.com/operable/go-relay/relay/bus"
	"github.com/operable/go-relay/relay/config"
	"github.com/operable/go-relay/relay/docker"
	"github.com/operable/go-relay/relay/worker"
)

const (
	BAD_CONFIG = iota + 1
	DOCKER_ERR
	BUS_ERR
)

var configFile = flag.String("file", "/etc/cog_relay.conf", "Path to configuration file")

func init() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFormatter(&log.TextFormatter{
		DisableTimestamp: false,
		FullTimestamp:    true,
		DisableSorting:   true,
	})
}

func configureLogger(config *config.Config) {
	if config.LogJSON == true {
		log.SetFormatter(&log.JSONFormatter{})
	}
	switch config.LogPath {
	case "stderr":
		log.SetOutput(os.Stderr)
	case "console":
		fallthrough
	case "stdout":
		log.SetOutput(os.Stdout)
	default:
		logFile, err := os.Open(config.LogPath)
		if err != nil {
			panic(err)
		}
		log.SetOutput(logFile)
	}
	switch config.LogLevel {
	case "debug":
		log.SetLevel(log.DebugLevel)
	case "info":
		log.SetLevel(log.InfoLevel)
	case "warn":
		log.SetLevel(log.WarnLevel)
	case "err":
		fallthrough
	case "error":
		log.SetLevel(log.ErrorLevel)
	default:
		os.Stderr.Write([]byte(fmt.Sprintf("Unknown log level '%s'. Defaulting to info.\n",
			config.LogLevel)))
		log.SetLevel(log.InfoLevel)
	}
}

func prepare() *config.Config {
	flag.Parse()
	config, err := config.LoadConfig(*configFile)
	if err != nil {
		errstr := fmt.Sprintf("%s", err)
		msgs := strings.Split(errstr, ";")
		log.Errorf("Error loading %s:", *configFile)
		for _, msg := range msgs {
			log.Errorf("  %s", msg)
		}
		os.Exit(BAD_CONFIG)
	}
	configureLogger(config)
	return config
}

func shutdown(config *config.Config, link worker.Service, workQueue *worker.Queue, coordinator sync.WaitGroup) {
	// Remove signal handler so Ctrl-C works
	signal.Reset(syscall.SIGINT)

	log.Info("Starting shut down.")

	// Stop message bus listeners
	if link != nil {
		link.Halt()
	}

	// Stop work queue
	workQueue.Stop()

	// Wait on processes to exit
	coordinator.Wait()
	log.Infof("Relay %s shut down complete.", config.ID)
}

func main() {
	var coordinator sync.WaitGroup
	incomingSignal := make(chan os.Signal, 1)

	// Set up signal handlers
	signal.Notify(incomingSignal, syscall.SIGINT)
	config := prepare()
	log.Infof("Configuration file %s loaded.", *configFile)
	log.Infof("Relay %s is initializing.", config.ID)

	// Create work queue with some burstable capacity
	workQueue := worker.NewQueue(config.MaxConcurrent * 2)

	if config.DockerDisabled == false {
		err := docker.VerifyConfig(config.Docker)
		if err != nil {
			log.Errorf("Error verifying Docker configuration: %s.", err)
			shutdown(config, nil, workQueue, coordinator)
			os.Exit(DOCKER_ERR)
		} else {
			log.Infof("Docker configuration verified.")
		}
	} else {
		log.Infof("Docker support disabled.")
	}

	// Start MaxConcurrent workers
	for i := 0; i < config.MaxConcurrent; i++ {
		go func() {
			worker.RunWorker(workQueue, coordinator)
		}()
	}
	log.Infof("Started %d workers.", config.MaxConcurrent)

	// Connect to Cog
	handler := func(bus worker.MessageBus, topic string, payload []byte) {
		return
	}
	subs := bus.Subscriptions{
		CommandHandler:   handler,
		ExecutionHandler: handler,
	}
	link, err := bus.NewLink(config.ID, config.Cog, workQueue, subs, coordinator)
	if err != nil {
		log.Errorf("Error connecting to Cog: %s.", err)
		shutdown(config, nil, workQueue, coordinator)
		os.Exit(BUS_ERR)
	}

	log.Infof("Connected to Cog host %s.", config.Cog.Host)
	err = link.Run()
	if err != nil {
		log.Errorf("Error subscribing to message topics: %s.", err)
		shutdown(config, nil, workQueue, coordinator)
		os.Exit(BUS_ERR)
	}
	log.Infof("Relay %s is ready.", config.ID)

	// Wait until we get a signal
	<-incomingSignal

	// Shutdown
	shutdown(config, link, workQueue, coordinator)
}

func handleMessage(queue *worker.Queue, config *config.Config, bus worker.MessageBus, topic string, payload []byte) {
	engine, err := newDockerEngine(config)
	if err != nil {
		log.Errorf("Error connecting to Docker: %s", err)
		//TODO Send error to Cog
		return
	}
	request := &worker.Request{
		Bus:          bus,
		DockerEngine: engine,
		Topic:        topic,
		Message:      payload,
	}
	queue.Enqueue(request)
}

func newDockerEngine(config *config.Config) (*docker.Engine, error) {
	if config.DockerDisabled == false {
		engine, err := docker.NewEngine(config.Docker)
		if err != nil {
			return engine, nil
		}
		return nil, err
	}
	return nil, nil
}
