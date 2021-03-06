package metricslite_test

import (
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/metricslite"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

func TestPrometheus(t *testing.T) {
	tests := []struct {
		name string
		fn   func(m metricslite.Interface)
		body string
	}{
		{
			name: "noop",
		},
		{
			name: "counters",
			fn:   testCounters,
			body: `# HELP bar_total A second counter.
# TYPE bar_total counter
bar_total 5
# HELP foo_total A counter.
# TYPE foo_total counter
foo_total{address="127.0.0.1",interface="eth0"} 1
foo_total{address="2001:db8::1",interface="eth1"} 1
foo_total{address="::1",interface="eth0"} 1
`,
		},
		{
			name: "gauges",
			fn:   testGauges,
			body: `# HELP bar_bytes A second gauge.
# TYPE bar_bytes gauge
bar_bytes{device="eth0"} 1024
# HELP foo_celsius A gauge.
# TYPE foo_celsius gauge
foo_celsius{probe="temp0"} 1
foo_celsius{probe="temp1"} 100
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := prometheus.NewPedanticRegistry()
			m := metricslite.NewPrometheus(reg)
			h := promhttp.HandlerFor(reg, promhttp.HandlerOpts{})

			if tt.fn != nil {
				tt.fn(m)
			}

			s := httptest.NewServer(h)
			defer s.Close()

			u, err := url.Parse(s.URL)
			if err != nil {
				t.Fatalf("failed to parse temporary HTTP server URL: %v", err)
			}

			client := &http.Client{Timeout: 1 * time.Second}
			res, err := client.Get(u.String())
			if err != nil {
				t.Fatalf("failed to perform HTTP request: %v", err)
			}
			defer res.Body.Close()

			// Set a sane upper limit on the number of bytes in the response body.
			const mebibyte = 1 << 20
			body, err := ioutil.ReadAll(io.LimitReader(res.Body, 16*mebibyte))
			if err != nil {
				t.Fatalf("failed to read HTTP response body: %v", err)
			}

			if diff := cmp.Diff(tt.body, string(body)); diff != "" {
				t.Fatalf("unexpected HTTP body (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMemory(t *testing.T) {
	tests := []struct {
		name  string
		fn    func(m metricslite.Interface)
		final map[string]metricslite.Series
	}{
		{
			name:  "noop",
			final: map[string]metricslite.Series{},
		},
		{
			name: "counters",
			fn:   testCounters,
			final: map[string]metricslite.Series{
				"bar_total": {
					Name:    "bar_total",
					Help:    "A second counter.",
					Samples: map[string]float64{"": 5},
				},
				"foo_total": {
					Name: "foo_total",
					Help: "A counter.",
					Samples: map[string]float64{
						"address=127.0.0.1,interface=eth0":   1,
						"address=2001:db8::1,interface=eth1": 1,
						"address=::1,interface=eth0":         1,
					},
				},
			},
		},
		{
			name: "gauges",
			fn:   testGauges,
			final: map[string]metricslite.Series{
				"bar_bytes": {
					Name:    "bar_bytes",
					Help:    "A second gauge.",
					Samples: map[string]float64{"device=eth0": 1024},
				},
				"foo_celsius": {
					Name: "foo_celsius",
					Help: "A gauge.",
					Samples: map[string]float64{
						"probe=temp0": 1,
						"probe=temp1": 100,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := metricslite.NewMemory()

			if tt.fn != nil {
				tt.fn(m)
			}

			if diff := cmp.Diff(tt.final, m.Series()); diff != "" {
				t.Fatalf("unexpected timeseries (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMemoryPanics(t *testing.T) {
	// TODO: test other Interface implementations for panics.

	tests := []struct {
		name string
		msg  string
		fn   func(m *metricslite.Memory)
	}{
		{
			name: "already registered",
			msg:  `metricslite: timeseries "foo_total" already registered`,
			fn: func(m *metricslite.Memory) {
				m.Counter("foo_total", "A counter.")
				m.Gauge("foo_total", "A gauge.")
			},
		},
		{
			name: "mismatched cardinality",
			msg:  `metricslite: mismatched label cardinality for timeseries "foo_total"`,
			fn: func(m *metricslite.Memory) {
				c := m.Counter("foo_total", "A counter.")
				c("panics")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, panics := panics(func() {
				tt.fn(metricslite.NewMemory())
			})
			if !panics {
				t.Fatal("test did not panic")
			}

			if diff := cmp.Diff(tt.msg, msg); diff != "" {
				t.Fatalf("unexpected panic message (-want +got):\n%s", diff)
			}
		})
	}
}

func TestMemoryConcurrent(t *testing.T) {
	// TODO: check concurrency safety of other metricslite.Interface implementations.

	var (
		m = metricslite.NewMemory()
		g = m.Gauge("foo", "", "foo")
	)

	const n = 8

	var wg sync.WaitGroup
	wg.Add(n)
	defer wg.Wait()

	waitC := make(chan struct{})
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			<-waitC

			for j := 0; j < 1000; j++ {
				g(float64(j), "bar")
				for _, v := range m.Series() {
					_ = v.Samples["foo"]
				}
			}
		}()
	}

	close(waitC)
}

func TestDiscardDoesNotPanic(t *testing.T) {
	tests := []struct {
		name string
		fn   func(m metricslite.Interface)
	}{
		{
			name: "counters",
			fn:   testCounters,
		},
		{
			name: "gauges",
			fn:   testGauges,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg, panics := panics(func() {
				tt.fn(metricslite.Discard())
			})
			if panics {
				t.Fatalf("test panicked: %s", msg)
			}
		})
	}
}

func testCounters(m metricslite.Interface) {
	var (
		c1 = m.Counter("foo_total", "A counter.", "address", "interface")
		c2 = m.Counter("bar_total", "A second counter.")
	)

	// A counter increments for each call with a given label set.
	c1("127.0.0.1", "eth0")
	c1("::1", "eth0")
	c1("2001:db8::1", "eth1")

	for i := 0; i < 5; i++ {
		c2()
	}
}

func testGauges(m metricslite.Interface) {
	var (
		g1 = m.Gauge("foo_celsius", "A gauge.", "probe")
		g2 = m.Gauge("bar_bytes", "A second gauge.", "device")
	)

	// A gauge only stores the last value for a given timeseries.
	g1(0, "temp0")
	g1(100, "temp1")
	g1(1, "temp0")

	g2(1024, "eth0")
}

func panics(fn func()) (msg string, panics bool) {
	defer func() {
		if r := recover(); r != nil {
			// On panic, capture the output string and return that fn did
			// panic.
			msg = r.(string)
			panics = true
		}
	}()

	fn()

	// fn did not panic.
	return "", false
}
