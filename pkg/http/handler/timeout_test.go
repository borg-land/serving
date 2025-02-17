/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package handler

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/clock"
)

func TestTimeoutWriterAllowsForAdditionalWritesBeforeTimeout(t *testing.T) {
	recorder := httptest.NewRecorder()
	clock := clock.RealClock{}
	handler := &timeoutWriter{w: recorder, clock: clock}
	handler.WriteHeader(http.StatusOK)
	handler.tryFirstByteTimeoutAndWriteError("error")
	handler.tryIdleTimeoutAndWriteError(clock.Now(), 10*time.Second, "error")
	if _, err := io.WriteString(handler, "test"); err != nil {
		t.Fatalf("handler.Write() = %v, want no error", err)
	}

	if got, want := recorder.Code, http.StatusOK; got != want {
		t.Errorf("recorder.Status = %d, want %d", got, want)
	}
	if got, want := recorder.Body.String(), "test"; got != want {
		t.Errorf("recorder.Body = %s, want %s", got, want)
	}
}

func TestTimeoutWriterDoesntFlushAfterTimeout(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := &timeoutWriter{w: recorder, clock: clock.RealClock{}}
	handler.timeoutAndWriteError("error")
	handler.Flush()

	if got, want := recorder.Flushed, false; got != want {
		t.Errorf("recorder.Flushed = %t, want %t", got, want)
	}
}

func TestTimeoutWriterFlushesBeforeTimeout(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := &timeoutWriter{w: recorder, clock: clock.RealClock{}}
	handler.Flush()

	if got, want := recorder.Flushed, true; got != want {
		t.Errorf("recorder.Flushed = %t, want %t", got, want)
	}
}

func TestTimeoutWriterErrorsWriteAfterTimeout(t *testing.T) {
	recorder := httptest.NewRecorder()
	handler := &timeoutWriter{w: recorder, clock: clock.RealClock{}}
	handler.timeoutAndWriteError("error")
	if _, err := handler.Write([]byte("hello")); !errors.Is(err, http.ErrHandlerTimeout) {
		t.Errorf("ErrHandlerTimeout got %v, want: %s", err, http.ErrHandlerTimeout)
	}
}

type timeoutHandlerTestScenario struct {
	name               string
	firstByteTimeout   time.Duration
	idleTimeout        time.Duration
	maxDurationTimeout time.Duration
	handler            func(clock *clock.FakeClock, mux *sync.Mutex, writeErrors chan error) http.Handler
	timeoutMessage     string
	wantStatus         int
	wantBody           string
	wantWriteError     bool
	wantPanic          bool
}

// This has to be global as the timer cache would otherwise return timers from another clock.
var fakeClock = clock.NewFakeClock(time.Time{})

func testTimeoutScenario(t *testing.T, scenarios []timeoutHandlerTestScenario) {
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)

			var reqMux sync.Mutex
			writeErrors := make(chan error, 1)
			rr := httptest.NewRecorder()

			handler := &timeoutHandler{
				handler:            scenario.handler(fakeClock, &reqMux, writeErrors),
				body:               scenario.timeoutMessage,
				firstByteTimeout:   scenario.firstByteTimeout,
				idleTimeout:        scenario.idleTimeout,
				maxDurationTimeout: scenario.maxDurationTimeout,
				clock:              fakeClock,
			}

			defer func() {
				if scenario.wantPanic {
					if recovered := recover(); recovered != http.ErrAbortHandler { //nolint // False positive for errors.Is check
						t.Errorf("Recover = %v, want: %v", recovered, http.ErrAbortHandler)
					}
				}
			}()

			reqMux.Lock() // Will cause an inner 'Lock' to block. ServeHTTP will exit early if the call times out.
			handler.ServeHTTP(rr, req)
			reqMux.Unlock() // Allows the inner 'Lock' to go through to complete potential writes.

			if status := rr.Code; status != scenario.wantStatus {
				t.Errorf("Handler returned wrong status code: got %v want %v", status, scenario.wantStatus)
			}

			if rr.Body.String() != scenario.wantBody {
				t.Errorf("Handler returned unexpected body: got %q want %q", rr.Body.String(), scenario.wantBody)
			}

			if scenario.wantWriteError {
				if err := <-writeErrors; !errors.Is(err, http.ErrHandlerTimeout) {
					t.Error("Expected a timeout error, got", err)
				}
			}
		})
	}
}

func TestTimeToFirstByteTimeoutHandler(t *testing.T) {
	const (
		immediateTimeout = 1 * time.Millisecond
		longTimeout      = 1 * time.Minute // Super long, not supposed to hit this.
		noIdleTimeout    = 0 * time.Millisecond
	)

	scenarios := []timeoutHandlerTestScenario{{
		name:             "all good",
		firstByteTimeout: longTimeout,
		idleTimeout:      noIdleTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hi"))
			})
		},
		wantStatus: http.StatusOK,
		wantBody:   "hi",
	}, {
		name:             "custom timeout message",
		firstByteTimeout: immediateTimeout,
		idleTimeout:      noIdleTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, writeErrors chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(immediateTimeout)
				mux.Lock()
				defer mux.Unlock()
				_, werr := w.Write([]byte("hi"))
				writeErrors <- werr
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusGatewayTimeout,
		wantBody:       "request timeout",
		wantWriteError: true,
	}, {
		name:             "propagate panic",
		firstByteTimeout: longTimeout,
		idleTimeout:      noIdleTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			})
		},
		wantStatus: http.StatusGatewayTimeout,
		wantBody:   "request timeout",
		wantPanic:  true,
	}, {
		name:             "timeout before panic",
		firstByteTimeout: immediateTimeout,
		idleTimeout:      noIdleTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(immediateTimeout)
				mux.Lock()
				defer mux.Unlock()
				panic(http.ErrAbortHandler)
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusGatewayTimeout,
		wantBody:       "request timeout",
		wantPanic:      false,
	}}
	testTimeoutScenario(t, scenarios)
}

func TestIdleTimeoutHandler(t *testing.T) {
	const (
		noIdleTimeout        = 0 * time.Millisecond
		immediateIdleTimeout = 1 * time.Millisecond // 0 would disable the timeout
		shortIdleTimeout     = 100 * time.Millisecond
		longIdleTimeout      = 1 * time.Minute // Super long, not supposed to hit this.
		longFirstByteTimeout = 1 * time.Minute // Super long, not supposed to hit this.
	)

	scenarios := []timeoutHandlerTestScenario{{
		name:             "all good",
		firstByteTimeout: longFirstByteTimeout,
		idleTimeout:      longIdleTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hi"))
			})
		},
		wantStatus: http.StatusOK,
		wantBody:   "hi",
	}, {
		name:             "custom timeout message",
		idleTimeout:      immediateIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, writeErrors chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(immediateIdleTimeout)
				mux.Lock()
				defer mux.Unlock()
				_, werr := w.Write([]byte("hi"))
				writeErrors <- werr
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusGatewayTimeout,
		wantBody:       "request timeout",
		wantWriteError: true,
	}, {
		name:             "propagate panic",
		idleTimeout:      longIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			})
		},
		wantStatus: http.StatusGatewayTimeout,
		wantBody:   "request timeout",
		wantPanic:  true,
	}, {
		name:             "timeout before panic",
		idleTimeout:      immediateIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(immediateIdleTimeout)
				mux.Lock()
				defer mux.Unlock()
				panic(http.ErrAbortHandler)
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusGatewayTimeout,
		wantBody:       "request timeout",
		wantPanic:      false,
	}, {
		name:             "successful writes prevent timeout",
		idleTimeout:      shortIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(c *clock.FakeClock, _ *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("foo"))
				c.Step(shortIdleTimeout - 1*time.Millisecond)
				w.Write([]byte("bar"))
				c.Step(shortIdleTimeout - 1*time.Millisecond)
				w.Write([]byte("baz"))
				c.Step(shortIdleTimeout - 1*time.Millisecond)
				w.Write([]byte("test"))
				c.Step(shortIdleTimeout - 1*time.Millisecond)
			})
		},
		wantStatus: http.StatusOK,
		wantBody:   "foobarbaztest",
		wantPanic:  false,
	}, {
		name:             "can still timeout after a successful write",
		idleTimeout:      shortIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("foo"))
				c.Step(shortIdleTimeout + 1*time.Millisecond)
				mux.Lock()
				defer mux.Unlock()
				panic(http.ErrAbortHandler)
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusOK,
		wantBody:       "foorequest timeout",
		wantPanic:      false,
	}, {
		name:             "no idle timeout",
		idleTimeout:      noIdleTimeout,
		firstByteTimeout: longFirstByteTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			})
		},
		wantStatus: http.StatusGatewayTimeout,
		wantBody:   "request timeout",
		wantPanic:  true,
	}}

	// Max duration is 0, thus disabled
	testTimeoutScenario(t, scenarios)

	// If max duration is set high enough it should not affect any idle timeout test
	for i := range scenarios {
		scenarios[i].maxDurationTimeout = 5 * longIdleTimeout
	}
	testTimeoutScenario(t, scenarios)
}

func TestMaxDurationTimeoutHandler(t *testing.T) {
	const (
		noTimeout        = 0 * time.Millisecond
		immediateTimeout = 1 * time.Millisecond
		shortTimeout     = 100 * time.Millisecond
		longTimeout      = 1 * time.Minute
	)

	scenarios := []timeoutHandlerTestScenario{{
		name:               "all good",
		firstByteTimeout:   shortTimeout,
		idleTimeout:        shortTimeout,
		maxDurationTimeout: longTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Write([]byte("hi"))
			})
		},
		wantStatus: http.StatusOK,
		wantBody:   "hi",
	}, {
		name:               "max duration timeout with writes",
		firstByteTimeout:   shortTimeout,
		idleTimeout:        shortTimeout,
		maxDurationTimeout: longTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(longTimeout)
				mux.Lock()
				defer mux.Unlock()
				w.Write([]byte("hi"))
			})
		},
		wantStatus: http.StatusGatewayTimeout,
	}, {
		name:               "propagate panic, max duration too short",
		idleTimeout:        longTimeout,
		firstByteTimeout:   longTimeout,
		maxDurationTimeout: immediateTimeout,
		handler: func(*clock.FakeClock, *sync.Mutex, chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				panic(http.ErrAbortHandler)
			})
		},
		wantStatus: http.StatusGatewayTimeout,
		wantBody:   "request timeout",
		wantPanic:  true,
	}, {
		name:               "timeout before panic, max duration too short",
		idleTimeout:        shortTimeout,
		firstByteTimeout:   shortTimeout,
		maxDurationTimeout: immediateTimeout,
		handler: func(c *clock.FakeClock, mux *sync.Mutex, _ chan error) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c.Step(immediateTimeout)
				mux.Lock()
				defer mux.Unlock()
				panic(http.ErrAbortHandler)
			})
		},
		timeoutMessage: "request timeout",
		wantStatus:     http.StatusGatewayTimeout,
		wantBody:       "request timeout",
		wantPanic:      false,
	}}

	testTimeoutScenario(t, scenarios)

}

func BenchmarkTimeoutHandler(b *testing.B) {
	writes := [][]byte{[]byte("this"), []byte("is"), []byte("a"), []byte("test")}
	baseHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		for _, write := range writes {
			w.Write(write)
		}
	})
	handler := NewTimeoutHandler(baseHandler, "test", 10*time.Minute, 10*time.Minute, 10*time.Minute)
	req := httptest.NewRequest(http.MethodPost, "http://example.com", nil)

	b.Run("sequential", func(b *testing.B) {
		resp := httptest.NewRecorder()
		for j := 0; j < b.N; j++ {
			handler.ServeHTTP(resp, req)
		}
	})

	b.Run("parallel", func(b *testing.B) {
		b.RunParallel(func(pb *testing.PB) {
			resp := httptest.NewRecorder()
			for pb.Next() {
				handler.ServeHTTP(resp, req)
			}
		})
	})
}
