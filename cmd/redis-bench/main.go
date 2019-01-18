package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"sync"
	"time"

	humanize "github.com/dustin/go-humanize"
	"github.com/emicklei/glog"
	bench "github.com/finn-no/redisbench"
)

var opts struct {
	addrs         commaSepString
	db            string // For HA
	port          string
	mode          bench.Mode
	connections   int
	clients       int
	methods       trafficMix
	cpuProfile    string
	pipeline      int
	poolPerClient bool
	statusPort    int
	end           struct {
		rpcs int64
		// Not yet implemented:
		duration time.Duration
	}
}

type trafficMix map[string]float64

func (m *trafficMix) String() string {
	return fmt.Sprint(*m)
}

func (m *trafficMix) Set(value string) error {
	if *m == nil {
		*m = trafficMix{}
	}
	for _, item := range strings.Split(value, ",") {
		x := strings.SplitN(item, ":", 2)
		method, amount := x[0], x[1]
		frac, err := strconv.ParseFloat(amount, 64)
		if err != nil {
			return err
		}
		(*m)[method] = frac
	}
	return nil
}

func (m *trafficMix) Get() interface{} {
	return trafficMix(*m)
}

type commaSepString []string

func (a *commaSepString) String() string {
	return strings.Join(*a, ",")
}
func (a *commaSepString) Set(s string) error {
	*a = strings.Split(s, ",")
	return nil
}

func init() {
	flag.Var(&opts.mode, "mode", "Replication technology ('single', 'ha', 'cluster')")
	flag.Var(&opts.addrs, "addrs", "Servers to connect to")
	flag.StringVar(&opts.db, "db", "", "Database name (for HA)")
	flag.StringVar(&opts.port, "port", "6379", "Default port to connect to")
	flag.IntVar(&opts.clients, "clients", 5, "Number of clients")
	flag.IntVar(&opts.connections, "connections", -1, "Size of the connection pool (-1: auto)")
	flag.BoolVar(&opts.poolPerClient, "poolPerClient", true, "Make a connection pool per client")
	flag.IntVar(&opts.pipeline, "pipeline", 0, "Number of commands to pipeline. 0: no pipelining")
	flag.Var(&opts.methods, "methods", "Traffic mix eg GET:50,PUT:50")
	flag.Int64Var(&opts.end.rpcs, "rpcs", 1000000, "Number of RPCs to send")
	flag.StringVar(&opts.cpuProfile, "cpuprofile", "", "Write CPU profile to this file")
	flag.IntVar(&opts.statusPort, "statusPort", 8080, "HTTP server port")
}

func maybeProfile() func() {
	if opts.cpuProfile == "" {
		return func() {}
	}
	if f, err := os.Create(opts.cpuProfile); err != nil {
		glog.Fatal(err)
	} else {
		pprof.StartCPUProfile(f)
	}
	return pprof.StopCPUProfile
}

func hostPorts(hosts []string) []string {
	ret := []string{}
	for _, h := range hosts {
		var hostname string
		if _, port, err := net.SplitHostPort(h); err != nil && port != "" {
			hostname = h
		} else {
			hostname = net.JoinHostPort(h, opts.port)
		}
		ret = append(ret, hostname)
	}
	glog.V(2).Infof("HostPorts <- %v\nHostPorts -> %v", hosts, ret)
	return ret
}

func setLogging() {
	output := os.Getenv("LOG_STDOUT")
	var logWriter io.Writer
	if strings.ToLower(output) == "true" {
		// Buffer stdout.
		logWriter = bufio.NewWriter(os.Stdout)
		// Use $LOG_STDOUT as a proxy for the question, "Are
		// we running in a container?" If so, we might not be
		// able to write to log _files_.
		flag.Set("logtostderr", "true")
	} else {
		// Don't buffer stderr.
		logWriter = os.Stderr
	}
	format := os.Getenv("LOG_FORMAT")
	if strings.ToLower(format) == "json" {
		glog.SetLogstashWriter(logWriter)
	}
}

func ServeHttp(port int) {
	if port == -1 {
		return
	}

	healthHandler := func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	}
	http.HandleFunc("/_/health", healthHandler)
	http.HandleFunc("/_/ready", healthHandler)

	go http.ListenAndServe(fmt.Sprintf(":%d", port), nil)
}

func main() {
	flag.Parse()
	glog.V(1).Infof("Opts %+v", opts)
	//	profDone := maybeProfile()
	//	defer profDone()
	runtime.SetMutexProfileFraction(5)

	ServeHttp(opts.statusPort)

	if len(opts.addrs) < 1 {
		fmt.Fprintf(os.Stderr, "No -addrs passed!\n\n")
		flag.Usage()
		os.Exit(2)
	}

	var modeFunc bench.LoadTestOpt
	switch opts.mode {
	case bench.SINGLE:
		modeFunc = bench.SingleMode(hostPorts(opts.addrs)[0])
	case bench.HA:
		modeFunc = bench.SentinelMode(opts.db, hostPorts(opts.addrs))
	default:
	}

	var connections int
	if opts.connections == -1 {
		connections = int(float64(opts.clients) * 1.5)
	} else {
		connections = opts.connections
	}
	ltOpts := []bench.LoadTestOpt{
		bench.SetConnections(connections),
		bench.SetPipeline(opts.pipeline),
		bench.SetPoolPerClient(opts.poolPerClient),
	}
	if len(opts.methods) > 0 {
		ltOpts = append(ltOpts, bench.SetTrafficMix(opts.methods))
	}
	l, err := bench.NewLoadTest(modeFunc, ltOpts...)
	if err != nil {
		panic(err)
	}
	started := time.Now()
	n := int64(opts.end.rpcs)
	watchDone := l.Watch()
	defer watchDone()
	finished := func() {
		l.Stop()
		done := time.Now()
		duration := done.Sub(started)
		n := l.Rpcs()
		glog.Infof("Ran %d RPCs in %v (%s, %s)",
			n, duration,
			humanize.SI(float64(n)/duration.Seconds(), "Hz"),
			humanize.SI(duration.Seconds()/float64(n), "s"))
	}
	once := sync.Once{}
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		<-c
		once.Do(finished)
		os.Exit(0)
	}()

	l.RunSeveral(opts.clients, n)
	once.Do(finished)
}
