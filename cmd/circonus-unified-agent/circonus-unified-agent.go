//go:build go1.17
// +build go1.17

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // G108 -- Comment this line to disable pprof endpoint.
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/circonus-labs/circonus-unified-agent/agent"
	"github.com/circonus-labs/circonus-unified-agent/config"
	"github.com/circonus-labs/circonus-unified-agent/internal"
	"github.com/circonus-labs/circonus-unified-agent/internal/circonus"
	"github.com/circonus-labs/circonus-unified-agent/internal/goplugin"
	"github.com/circonus-labs/circonus-unified-agent/internal/release"
	"github.com/circonus-labs/circonus-unified-agent/logger"
	_ "github.com/circonus-labs/circonus-unified-agent/plugins/aggregators/all"
	"github.com/circonus-labs/circonus-unified-agent/plugins/inputs"
	_ "github.com/circonus-labs/circonus-unified-agent/plugins/inputs/all"
	"github.com/circonus-labs/circonus-unified-agent/plugins/outputs"
	_ "github.com/circonus-labs/circonus-unified-agent/plugins/outputs/all"
	_ "github.com/circonus-labs/circonus-unified-agent/plugins/processors/all"
)

// If you update these, update usage.go and usage_windows.go
var fDebug = flag.Bool("debug", false,
	"turn on debug logging")
var pprofAddr = flag.String("pprof-addr", "",
	"pprof address to listen on, not activate pprof if empty")
var fQuiet = flag.Bool("quiet", false,
	"run in quiet mode")
var fTest = flag.Bool("test", false,
	"enable test mode: gather metrics, print them out, and exit. Note: Test mode only runs inputs, not processors, aggregators, or outputs")
var fTestWait = flag.Int("test-wait", 0,
	"wait up to this many seconds for service inputs to complete in test mode")
var fConfig = flag.String("config", "",
	"configuration file to load")
var fConfigDirectory = flag.String("config-directory", "",
	"directory containing additional *.conf files")
var fVersion = flag.Bool("version", false,
	"display the version and exit")
var fSampleConfig = flag.Bool("sample-config", false,
	"print out full sample configuration")
var fPidfile = flag.String("pidfile", "",
	"file to write our pid to")
var fSectionFilters = flag.String("section-filter", "",
	"filter the sections to print, separator is ':'. Valid values are 'agent', 'global_tags', 'outputs', 'processors', 'aggregators' and 'inputs'")
var fInputFilters = flag.String("input-filter", "",
	"filter the inputs to enable, separator is :")
var fInputList = flag.Bool("input-list", false,
	"print available input plugins.")
var fOutputFilters = flag.String("output-filter", "",
	"filter the outputs to enable, separator is :")
var fOutputList = flag.Bool("output-list", false,
	"print available output plugins.")
var fAggregatorFilters = flag.String("aggregator-filter", "",
	"filter the aggregators to enable, separator is :")
var fProcessorFilters = flag.String("processor-filter", "",
	"filter the processors to enable, separator is :")
var fUsage = flag.String("usage", "",
	"print usage for a plugin, ie, 'circonus-unified-agent --usage mysql'")
var fPlugins = flag.String("plugin-directory", "",
	"path to directory containing external plugins")
var fRunOnce = flag.Bool("once", false,
	"run one gather and exit")

var (
	version   string
	commit    string
	branch    string
	buildDate string
	buildTag  string
)

var stop chan struct{}

func reloadLoop(
	inputFilters []string,
	outputFilters []string,
	aggregatorFilters []string, //nolint:unparam
	processorFilters []string, //nolint:unparam
) {
	reload := make(chan bool, 1)
	reload <- true
	for <-reload {
		reload <- false

		ctx, cancel := context.WithCancel(context.Background())

		signals := make(chan os.Signal, 1)
		signal.Notify(signals, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGINT)
		go func() {
			select {
			case sig := <-signals:
				if sig == syscall.SIGHUP {
					log.Printf("I! Reloading config")
					<-reload
					reload <- true
				}
				cancel()
			case <-stop:
				cancel()
			}
		}()

		err := runAgent(ctx, inputFilters, outputFilters)
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Fatalf("E! [circonus-unified-agent] Error running agent: %v", err)
		}
	}
}

func runAgent(ctx context.Context,
	inputFilters []string,
	outputFilters []string,
) error {
	log.Printf("I! Starting Circonus Unified Agent %s", version)

	// If no other options are specified, load the config file and run.
	c := config.NewConfig()
	c.OutputFilters = outputFilters
	c.InputFilters = inputFilters
	err := c.LoadConfig(*fConfig)
	if err != nil {
		return fmt.Errorf("loadconfig (%s): %w", *fConfig, err)
	}

	if *fConfigDirectory != "" {
		err = c.LoadDirectory(*fConfigDirectory)
		if err != nil {
			return fmt.Errorf("loaddir (%s): %w", *fConfigDirectory, err)
		}
		log.Printf("I! Completed loading configs from %s", *fConfigDirectory)
	}

	// mgm: add default plugins and agent plugins
	if err := c.LoadDefaultPlugins(); err != nil {
		return fmt.Errorf("loading defaults: %w", err)
	}
	// mgm: initialize the internal circonus config.
	if err := circonus.Initialize(c.GetGlobalCirconusConfig()); err != nil {
		log.Fatalf("E! unable to initialize circonus %s", err)
	}
	if len(c.Tags) > 0 {
		circonus.AddGlobalTags(c.Tags)
	}

	if !*fTest && len(c.Outputs) == 0 {
		return fmt.Errorf("Error: no outputs found, did you provide a valid config file?")
	}
	if *fPlugins == "" && len(c.Inputs) == 0 {
		return fmt.Errorf("Error: no inputs found, did you provide a valid config file?")
	}

	if int64(c.Agent.Interval.Duration) <= 0 {
		return fmt.Errorf("Agent interval must be positive, found %s", c.Agent.Interval.Duration)
	}

	if int64(c.Agent.FlushInterval.Duration) <= 0 {
		return fmt.Errorf("Agent flush_interval must be positive; found %s", c.Agent.Interval.Duration)
	}

	ag, err := agent.NewAgent(c)
	if err != nil {
		return fmt.Errorf("new agent: %w", err)
	}

	// Setup logging as configured.
	logConfig := logger.LogConfig{
		Debug:               ag.Config.Agent.Debug || *fDebug,
		Quiet:               ag.Config.Agent.Quiet || *fQuiet,
		LogTarget:           ag.Config.Agent.LogTarget,
		Logfile:             ag.Config.Agent.Logfile,
		RotationInterval:    ag.Config.Agent.LogfileRotationInterval,
		RotationMaxSize:     ag.Config.Agent.LogfileRotationMaxSize,
		RotationMaxArchives: ag.Config.Agent.LogfileRotationMaxArchives,
	}

	logger.SetupLogging(logConfig)

	if *fRunOnce {
		wait := time.Duration(*fTestWait) * time.Second
		return ag.Once(ctx, wait)
	}

	if *fTest || *fTestWait != 0 {
		wait := time.Duration(*fTestWait) * time.Second
		return ag.Test(ctx, wait)
	}

	log.Printf("I! Loaded inputs: %s", strings.Join(c.InputNames(), " "))
	log.Printf("I! Loaded aggregators: %s", strings.Join(c.AggregatorNames(), " "))
	log.Printf("I! Loaded processors: %s", strings.Join(c.ProcessorNames(), " "))
	log.Printf("I! Loaded outputs: %s", strings.Join(c.OutputNames(), " "))
	log.Printf("I! Tags enabled: %s", c.ListTags())

	if *fPidfile != "" {
		f, err := os.OpenFile(*fPidfile, os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("E! Unable to create pidfile: %s", err)
		} else {
			fmt.Fprintf(f, "%d\n", os.Getpid())

			f.Close()

			defer func() {
				err := os.Remove(*fPidfile)
				if err != nil {
					log.Printf("E! Unable to remove pidfile: %s", err)
				}
			}()
		}
	}

	return ag.Run(ctx)
}

func usageExit(rc int) {
	fmt.Println(internal.Usage) //nolint:govet
	os.Exit(rc)
}

func formatFullVersion() string {
	var parts = []string{"circonus-unified-agent"}

	if version != "" {
		parts = append(parts, version)
	} else {
		parts = append(parts, "unknown")
	}

	if branch != "" || commit != "" || buildTag != "" {
		if branch == "" {
			branch = "unknown"
		}
		if commit == "" {
			commit = "unknown"
		}
		if buildTag == "" {
			buildTag = "n/a"
		}
		git := fmt.Sprintf("(git: %s %s %s)", branch, commit, buildTag)
		parts = append(parts, git)
	}

	if buildDate != "" {
		parts = append(parts, "built:"+buildDate)
	}

	return strings.Join(parts, " ")
}

func setReleaseInfo() {
	ri := release.GetInfo()

	version = ri.Version
	commit = ri.Commit
	branch = ri.Branch
	buildDate = ri.BuildDate
	buildTag = ri.BuildTag
}

func main() {
	setReleaseInfo()

	flag.Usage = func() { usageExit(0) }
	flag.Parse()
	args := flag.Args()

	sectionFilters, inputFilters, outputFilters := []string{}, []string{}, []string{}
	if *fSectionFilters != "" {
		sectionFilters = strings.Split(":"+strings.TrimSpace(*fSectionFilters)+":", ":")
	}
	if *fInputFilters != "" {
		inputFilters = strings.Split(":"+strings.TrimSpace(*fInputFilters)+":", ":")
	}
	if *fOutputFilters != "" {
		outputFilters = strings.Split(":"+strings.TrimSpace(*fOutputFilters)+":", ":")
	}

	aggregatorFilters, processorFilters := []string{}, []string{}
	if *fAggregatorFilters != "" {
		aggregatorFilters = strings.Split(":"+strings.TrimSpace(*fAggregatorFilters)+":", ":")
	}
	if *fProcessorFilters != "" {
		processorFilters = strings.Split(":"+strings.TrimSpace(*fProcessorFilters)+":", ":")
	}

	logger.SetupLogging(logger.LogConfig{})

	// Load external plugins, if requested.
	if *fPlugins != "" {
		log.Printf("I! Loading external plugins from: %s", *fPlugins)
		if err := goplugin.LoadExternalPlugins(*fPlugins); err != nil {
			log.Fatal("E! " + err.Error())
		}
	}

	if *pprofAddr != "" {
		go func() {
			pprofHostPort := *pprofAddr
			parts := strings.Split(pprofHostPort, ":")
			if len(parts) == 2 && parts[0] == "" {
				pprofHostPort = fmt.Sprintf("localhost:%s", parts[1])
			}
			pprofHostPort = "http://" + pprofHostPort + "/debug/pprof"

			log.Printf("I! Starting pprof HTTP server at: %s", pprofHostPort)

			err := http.ListenAndServe(*pprofAddr, nil) // #nosec G114 // G114: Use of net/http serve function that has no support for setting timeouts
			if err != nil {
				log.Fatal("E! " + err.Error())
			}
		}()
	}

	if len(args) > 0 {
		switch args[0] {
		case "version":
			fmt.Println(formatFullVersion())
			return
		case "config":
			config.PrintSampleConfig(
				sectionFilters,
				inputFilters,
				outputFilters,
				aggregatorFilters,
				processorFilters,
			)
			return
		}
	}

	// switch for flags which just do something and exit immediately
	switch {
	case *fOutputList:
		fmt.Println("Available Output Plugins: ")
		names := make([]string, 0, len(outputs.Outputs))
		for k := range outputs.Outputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %s\n", k)
		}
		return
	case *fInputList:
		fmt.Println("Available Input Plugins:")
		names := make([]string, 0, len(inputs.Inputs))
		for k := range inputs.Inputs {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			fmt.Printf("  %s\n", k)
		}
		return
	case *fVersion:
		fmt.Println(formatFullVersion())
		return
	case *fSampleConfig:
		config.PrintSampleConfig(
			sectionFilters,
			inputFilters,
			outputFilters,
			aggregatorFilters,
			processorFilters,
		)
		return
	case *fUsage != "":
		err := config.PrintInputConfig(*fUsage)
		err2 := config.PrintOutputConfig(*fUsage)
		if err != nil && err2 != nil {
			log.Fatalf("E! %s and %s", err, err2)
		}
		return
	}

	shortVersion := version
	if shortVersion == "" {
		shortVersion = "unknown"
	}

	// Configure version
	if err := internal.SetVersion(shortVersion); err != nil {
		log.Println("circonus-unified-agent version already configured to: " + internal.Version())
	}

	run(
		inputFilters,
		outputFilters,
		aggregatorFilters,
		processorFilters,
	)
}
