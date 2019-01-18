package redisbench

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emicklei/glog"
	"github.com/mediocregopher/radix"
	"golang.org/x/time/rate"
)

type LoadTest struct {
	loadTestOpts

	doneChans []chan (struct{})

	// vvv Protected by mutex statLock
	failures int64
	rpcs     int64
	counters map[string]int64
	// ^^^ Protected by mutex statLock
	statLock sync.Mutex
}

type loadTestOpts struct {
	mode            Mode
	addresses       []string
	dbName          string // For HA
	keyPrefix       string
	keyDistribution Distribution
	ttlMilis        int64
	rpcRate         rate.Limit
	connections     int
	pipeline        int
	cumTrafficMix   []cumulativeTrafficShare
	pool            radix.Client
	poolPerClient   bool
	limiter         *rate.Limiter
}

type LoadTestOpt func(*loadTestOpts)

func SetPoolPerClient(sense bool) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.poolPerClient = sense
	}
}

func SetPipeline(n int) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.pipeline = n
	}
}

func SetConnections(n int) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.connections = n
	}
}

func SetSeed(seed int64) LoadTestOpt {
	return func(o *loadTestOpts) {
		rand.Seed(seed)
	}
}

type Mode int

const (
	SINGLE Mode = iota
	HA
	CLUSTER
)

func (m *Mode) String() string {
	return []string{"single", "ha", "cluster"}[*m]
}

func (m *Mode) Set(s string) error {
	val, ok := map[string]Mode{"single": SINGLE, "ha": HA, "cluster": CLUSTER}[s]
	if !ok {
		return errors.New("Mode not found")
	}
	*m = val
	return nil
}

func (m *Mode) Get() interface{} {
	return *m
}

func SetMode(m Mode) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.mode = m
	}
}

type cumulativeTrafficShare struct {
	float64
	string
}

func SetTrafficMix(mix map[string]float64) LoadTestOpt {
	return func(o *loadTestOpts) {
		var total, sum float64
		cdf := []cumulativeTrafficShare{}
		for _, v := range mix {
			total += v
		}
		for k, v := range mix {
			v /= total
			sum += v
			cdf = append(cdf, cumulativeTrafficShare{sum, k})
		}
		o.cumTrafficMix = cdf
	}
}

func SingleMode(addr string) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.mode = SINGLE
		o.addresses = []string{addr}
	}
}

func SentinelMode(dbName string, addrs []string) LoadTestOpt {
	return func(o *loadTestOpts) {
		o.mode = HA
		o.dbName = dbName
		o.addresses = addrs
	}
}

func NewLoadTest(modeFunc LoadTestOpt, opts ...LoadTestOpt) (*LoadTest, error) {
	l := &LoadTest{
		loadTestOpts: loadTestOpts{
			keyDistribution: FlatDistribution{},
			rpcRate:         rate.Inf,
			ttlMilis:        1000,
			limiter:         rate.NewLimiter(rate.Inf, 1),
		},
	}
	atomic.StoreInt64(&l.rpcs, 0)
	defaultOpts := []LoadTestOpt{
		modeFunc,
		SetSeed(time.Now().UnixNano()),
		SetTrafficMix(map[string]float64{"PING": 1.0}),
	}
	allOpts := append(defaultOpts, opts...)
	for _, o := range allOpts {
		o(&l.loadTestOpts)
	}
	if pool, err := l.clientFunction(); err != nil {
		return nil, err
	} else {
		l.pool = pool
	}
	glog.V(1).Infof("NewLoadTest: %+v", l)
	glog.V(1).Infof("NewLoadTest pool: %+v", l.pool)
	return l, nil
}

func (l *LoadTest) clientFunction() (radix.Client, error) {
	switch l.mode {
	case SINGLE:
		return radix.NewPool("tcp", l.addresses[0], l.connections)
	case HA:
		return radix.NewSentinel(l.dbName, l.addresses)
	default:
		return nil, fmt.Errorf("Mode %v not supported", l.mode)
	}
}

func (l *LoadTest) method() string {
	x := rand.Float64()
	for _, share := range l.cumTrafficMix {
		if share.float64 >= x {
			return share.string
		}
	}
	// This shouldn't happen but PING should be a benign command.
	return "PING"
}

func (l *LoadTest) key() string {
	return l.keyPrefix + "foo"
}

func (l *LoadTest) val() string {
	return "bar"
}

func (l *LoadTest) ttl() string {
	return string(l.ttlMilis)
}

// cmd takes a method name and RET, a pointer to be filled by the Action.
func (l *LoadTest) cmd(method string, ret *string) radix.CmdAction {
	type slot func() string
	var (
		SLOT_KEY   slot = l.key
		SLOT_VALUE slot = l.val
		SLOT_TTL   slot = l.ttl
	)
	slots := map[string][]slot{
		"PING":   []slot{SLOT_KEY},
		"GET":    []slot{SLOT_KEY},
		"SET":    []slot{SLOT_KEY, SLOT_VALUE},
		"EXPIRE": []slot{SLOT_KEY, SLOT_TTL},
		"SETEX":  []slot{SLOT_KEY, SLOT_TTL, SLOT_VALUE},
	}
	args := []string{}
	thisCmdSlots, ok := slots[method]
	if !ok {
		// Unrecognised command: panic!
		panic(fmt.Sprintf("Method '%s' not recognised!", method))
	}
	for _, sl := range thisCmdSlots {
		args = append(args, sl())
	}
	return radix.Cmd(ret, method, args...)
}

func (l *LoadTest) Watch() func() {
	done := make(chan bool)
	go func() {
		ticker := time.NewTicker(100 * time.Millisecond)
		var rpcs int64
		for {
			select {
			case <-done:
				ticker.Stop()
				close(done)
				return
			case <-ticker.C:
				totalRpcsNow := l.Rpcs()
				thisInterval := totalRpcsNow - rpcs
				rpcs = totalRpcsNow
				glog.Infof("%d RPCs (%d total)",
					thisInterval, rpcs)
			}
		}
	}()
	return func() {
		done <- true
	}
}

func (l *LoadTest) Rpcs() int64 {
	return atomic.LoadInt64(&l.rpcs)
}

// Run sends N RPCs to the Redis service, in l.TrafficMix fraction.
func (l *LoadTest) RunOne(n int64, finish chan struct{}) error {
	glog.V(2).Infof("LoadTest.Run() -- object: %+v", l)
	if l.pool == nil {
		return errors.New("pool not established: make LoadTest with NewLoadTest()")
	}

	var pool radix.Client
	if l.poolPerClient {
		var err error
		pool, err = l.clientFunction()
		if err != nil {
			return err
		}
	} else {
		pool = l.pool
	}

	loopRpcs := 0
	if l.pipeline <= 0 {
		loopRpcs = 1
	} else {
		loopRpcs = l.pipeline
	}
	for i := int64(0); i < n; i += int64(loopRpcs) {
		select {
		case <-finish:
			return nil
		default:
		}
		var pipeCmds []radix.CmdAction
		var cmd radix.Action
		if l.pipeline <= 0 {
			var ret string
			cmd = l.cmd(l.method(), &ret)
		} else {
			for j := 0; j < l.pipeline; j++ {
				var ret string
				cmd := l.cmd(l.method(), &ret)
				glog.V(3).Infof("cmd: %+v", cmd)
				pipeCmds = append(pipeCmds, cmd)
			}
			cmd = radix.Pipeline(pipeCmds...)
		}
		glog.V(2).Infof("Cmd: %+v", cmd)
		if err := pool.Do(cmd); err != nil {
			glog.V(1).Infof("Error %v while running %v", err, cmd)
			atomic.AddInt64(&l.failures, 1)
		}
		atomic.AddInt64(&l.rpcs, int64(loopRpcs))
	}
	return nil
}

// divideEqually returns []int of B buckets, as equal as possible, summing to N.
func divideEqually(n int64, b int) (buckets []int64) {
	d, m := n/int64(b), n%int64(b)
	for i := 0; i < b; i++ {
		var extra int64 = 0
		if int64(i) < m {
			extra = 1
		}
		buckets = append(buckets, d+extra)
	}
	return
}

func (l *LoadTest) RunSeveral(clients int, n int64) {
	b := divideEqually(n, clients)
	c := make(chan struct{})
	var tot int64
	for i := 0; i < clients; i++ {
		tot += b[i]
		// Avoid a spot of lock contention.
		time.Sleep(time.Millisecond * 2)
		go func(n int64) {
			done := make(chan struct{})
			l.doneChans = append(l.doneChans, done)
			l.RunOne(n, done)
			c <- struct{}{}
		}(b[i])
	}
	for i := 0; i < clients; i++ {
		<-c
	}
	l.doneChans = []chan (struct{}){}
}

func (l *LoadTest) Stop() {
	for _, c := range l.doneChans {
		c <- struct{}{}
	}
}
