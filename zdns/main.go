/*
 * ZDNS Copyright 2016 Regents of the University of Michigan
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you may not
 * use this file except in compliance with the License. You may obtain a copy
 * of the License at http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
 * implied. See the License for the specific language governing
 * permissions and limitations under the License.
 */

package main

import (
	"flag"
	"os"
	"runtime"
	"strings"
	"time"
	"io/ioutil"
	"math/rand"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
	"github.com/zmap/zdns"
	_ "github.com/zmap/zdns/modules/alookup"
	_ "github.com/zmap/zdns/modules/axfr"
	_ "github.com/zmap/zdns/modules/dmarc"
	_ "github.com/zmap/zdns/modules/miekg"
	_ "github.com/zmap/zdns/modules/mxlookup"
	_ "github.com/zmap/zdns/modules/nslookup"
	_ "github.com/zmap/zdns/modules/spf"

	_ "github.com/zmap/zdns/iohandlers/file"
)

func main() {

	var gc zdns.GlobalConf
	// global flags relevant to every lookup module
	flags := flag.NewFlagSet("flags", flag.ExitOnError)
	flags.IntVar(&gc.Threads, "threads", 1000, "number of lightweight go threads")
	flags.IntVar(&gc.GoMaxProcs, "go-processes", 0, "number of OS processes (GOMAXPROCS)")
	flags.StringVar(&gc.NamePrefix, "prefix", "", "name to be prepended to what's passed in (e.g., www.)")
	flags.BoolVar(&gc.AlexaFormat, "alexa", false, "is input file from Alexa Top Million download")
	flags.BoolVar(&gc.IterativeResolution, "iterative", false, "Perform own iteration instead of relying on recursive resolver")
	flags.StringVar(&gc.InputFilePath, "input-file", "-", "names to read")
	flags.StringVar(&gc.OutputFilePath, "output-file", "-", "where should JSON output be saved")
	flags.StringVar(&gc.MetadataFilePath, "metadata-file", "", "where should JSON metadata be saved")
	flags.StringVar(&gc.LogFilePath, "log-file", "", "where should JSON logs be saved")

	flags.StringVar(&gc.ResultVerbosity, "result-verbosity", "normal", "Sets verbosity of each output record. Options: short, normal, long, trace")
	flags.StringVar(&gc.IncludeInOutput, "include-fields", "", "Comma separated list of fields to additionally output beyond result verbosity. Options: class, protocol, ttl, resolver, flags")

	flags.IntVar(&gc.Verbosity, "verbosity", 3, "log verbosity: 1 (lowest)--5 (highest)")
	flags.IntVar(&gc.Retries, "retries", 1, "how many times should zdns retry query if timeout or temporary failure")
	flags.IntVar(&gc.MaxDepth, "max-depth", 10, "how deep should we recurse when performing iterative lookups")
	flags.IntVar(&gc.CacheSize, "cache-size", 10000, "how many items can be stored in internal recursive cache")
	flags.StringVar(&gc.InputHandler, "input-handler", "file", "handler to input names")
	flags.StringVar(&gc.OutputHandler, "output-handler", "file", "handler to output names")
	flags.BoolVar(&gc.TCPOnly, "tcp-only", false, "Only perform lookups over TCP")
	flags.BoolVar(&gc.UDPOnly, "udp-only", false, "Only perform lookups over UDP")
	servers_string := flags.String("name-servers", "", "List of DNS servers to use. Can be passed as comma-delimited string or via @/path/to/file. If no port is specified, defaults to 53.")
	config_file := flags.String("conf-file", "/etc/resolv.conf", "config file for DNS servers")
	timeout := flags.Int("timeout", 15, "timeout for resolving an individual name")
	iterationTimeout := flags.Int("iteration-timeout", 4, "timeout for resolving a single iteration in an iterative query")
	class_string := flags.String("class", "INET", "DNS class to query. Options: INET, CSNET, CHAOS, HESIOD, NONE, ANY. Default: INET.")
	nanoSeconds := flags.Bool("nanoseconds", false, "Use nanosecond resolution timestamps")
	// allow module to initialize and add its own flags before we parse
	if len(os.Args) < 2 {
		log.Fatal("No lookup module specified. Valid modules: ", zdns.ValidlookupsString())
	}
	gc.Module = strings.ToUpper(os.Args[1])
	factory := zdns.GetLookup(gc.Module)
	if factory == nil {
		flags.Parse(os.Args[1:])
		log.Fatal("Invalid lookup module specified. Valid modules: ", zdns.ValidlookupsString())
	}
	factory.AddFlags(flags)
	flags.Parse(os.Args[2:])
	// Do some basic sanity checking
	// setup global logging
	if gc.LogFilePath != "" {
		f, err := os.OpenFile(gc.LogFilePath, os.O_WRONLY|os.O_CREATE, 0666)
		if err != nil {
			log.Fatalf("Unable to open log file (%s): %s", gc.LogFilePath, err.Error())
		}
		log.SetOutput(f)
	}
	// Translate the assigned verbosity level to a logrus log level.
	switch gc.Verbosity {
	case 1: // Fatal
		log.SetLevel(log.FatalLevel)
	case 2: // Error
		log.SetLevel(log.ErrorLevel)
	case 3: // Warnings  (default)
		log.SetLevel(log.WarnLevel)
	case 4: // Information
		log.SetLevel(log.InfoLevel)
	case 5: // Debugging
		log.SetLevel(log.DebugLevel)
	default:
		log.Fatal("Unknown verbosity level specified. Must be between 1 (lowest)--5 (highest)")
	}
	// complete post facto global initialization based on command line arguments
	gc.Timeout = time.Duration(time.Second * time.Duration(*timeout))
	gc.IterationTimeout = time.Duration(time.Second * time.Duration(*iterationTimeout))
	// class initialization
	switch strings.ToUpper(*class_string) {
	case "INET", "IN":
		gc.Class = dns.ClassINET
	case "CSNET", "CS":
		gc.Class = dns.ClassCSNET
	case "CHAOS", "CH":
		gc.Class = dns.ClassCHAOS
	case "HESIOD", "HS":
		gc.Class = dns.ClassHESIOD
	case "NONE":
		gc.Class = dns.ClassNONE
	case "ANY":
		gc.Class = dns.ClassANY
	default:
		log.Fatal("Unknown record class specified. Valid valued are INET (default), CSNET, CHAOS, HESIOD, NONE, ANY")

	}
	if *servers_string == "" {
		// if we're doing recursive resolution, figure out default OS name servers
		// otherwise, use the set of 13 root name servers
		if gc.IterativeResolution {
			gc.NameServers = zdns.RootServers[:]
		} else {
			ns, err := zdns.GetDNSServers(*config_file)
			if err != nil {
				log.Fatal("Unable to fetch correct name servers:", err.Error())
			}
			gc.NameServers = ns
		}
		gc.NameServersSpecified = false
		log.Info("no name servers specified. will use: ", strings.Join(gc.NameServers, ", "))
	} else {
		var ns []string
		if (*servers_string)[0] == '@' {
			filepath := (*servers_string)[1:]
			f, err := ioutil.ReadFile(filepath)
			if err != nil {
				log.Fatalf("Unable to read file (%s): %s", filepath, err.Error())
			}
			if len(f) == 0 {
				log.Fatalf("Emtpy file (%s)", filepath)
			}
			ns = strings.Split(strings.Trim(string(f), "\n"), "\n")
		} else {
			ns = strings.Split(*servers_string, ",")
		}
		for i, s := range ns {
			if !strings.Contains(s, ":") {
				ns[i] = strings.TrimSpace(s) + ":53"
			}
		}
		gc.NameServers = ns
		gc.NameServersSpecified = true
	}
	if *nanoSeconds {
		gc.TimeFormat = time.RFC3339Nano
	} else {
		gc.TimeFormat = time.RFC3339
	}
	if gc.GoMaxProcs < 0 {
		log.Fatal("Invalid argument for --go-processes. Must be >1.")
	}
	if gc.GoMaxProcs != 0 {
		runtime.GOMAXPROCS(gc.GoMaxProcs)
	}
	if gc.UDPOnly && gc.TCPOnly {
		log.Fatal("TCP Only and UDP Only are conflicting")
	}
	// Output Groups are defined by a base + any additional fields that the user wants
	groups := strings.Split(gc.IncludeInOutput, ",")
	if gc.ResultVerbosity != "short" && gc.ResultVerbosity != "normal" && gc.ResultVerbosity != "long" && gc.ResultVerbosity != "trace" {
		log.Fatal("Invalid result verbosity. Options: short, normal, long, trace")
	}

	gc.OutputGroups = append(gc.OutputGroups, gc.ResultVerbosity)
	gc.OutputGroups = append(gc.OutputGroups, groups...)

	if len(flags.Args()) > 0 {
		stat, _ := os.Stdin.Stat()
		// If stdin is piped from the terminal, and we havent specified a file, and if we have unparsed args
		// use them for a dig like reslution
		if (stat.Mode()&os.ModeCharDevice) != 0 && gc.InputFilePath == "-" && len(flags.Args()) == 1 {
			gc.PassedName = flags.Args()[0]
		} else {
			log.Fatal("Unused command line flags: ", flags.Args())
		}
	}

	// Seeding for RandomNameServer()
	rand.Seed(time.Now().UnixNano())
	
	// some modules require multiple passes over a file (this is really just the case for zone files)
	if !factory.AllowStdIn() && gc.InputFilePath == "-" {
		log.Fatal("Specified module does not allow reading from stdin")
	}

	// allow the factory to initialize itself
	if err := factory.Initialize(&gc); err != nil {
		log.Fatal("Factory was unable to initialize:", err.Error())
	}
	// run it.
	if err := zdns.DoLookups(&factory, &gc); err != nil {
		log.Fatal("Unable to run lookups:", err.Error())
	}
	// allow the factory to initialize itself
	if err := factory.Finalize(); err != nil {
		log.Fatal("Factory was unable to finalize:", err.Error())
	}
}
