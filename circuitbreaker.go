// Package circuit implements the Circuit Breaker pattern. It will wrap
// a function call (typically one which uses remote services) and monitors for
// failures and/or time outs. When a threshold of failures or time outs has been
// reached, future calls to the function will not run. During this state, the
// breaker will periodically allow the function to run and, if it is successful,
// will start running the function again.
//
// Circuit includes three types of circuit breakers:
//
// A Threshold Breaker will trip when the failure count reaches a given threshold.
// It does not matter how long it takes to reach the threshold and the failures do
// not need to be consecutive.
//
// A Consecutive Breaker will trip when the consecutive failure count reaches a given
// threshold. It does not matter how long it takes to reach the threshold, but the
// failures do need to be consecutive.
//
//
// When wrapping blocks of code with a Breaker's Call() function, a time out can be
// specified. If the time out is reached, the breaker's Fail() function will be called.
//
//
// Other types of circuit breakers can be easily built by creating a Breaker and
// adding a custom TripFunc. A TripFunc is called when a Breaker Fail()s and receives
// the breaker as an argument. It then returns true or false to indicate whether the
// breaker should trip.
//
// The package also provides a wrapper around an http.Client that wraps all of
// the http.Client functions with a Breaker.
//
package circuit

import (
	"container/ring"
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cenkalti/backoff"
	"github.com/facebookgo/clock"
)

// BreakerEvent indicates the type of event received over an event channel
type BreakerEvent int

const (
	// BreakerTripped is sent when a breaker trips
	BreakerTripped BreakerEvent = iota

	// BreakerReset is sent when a breaker resets
	BreakerReset BreakerEvent = iota

	// BreakerFail is sent when Fail() is called
	BreakerFail BreakerEvent = iota

	// BreakerReady is sent when the breaker enters the half open state and is ready to retry
	BreakerReady BreakerEvent = iota
)

// ListenerEvent includes a reference to the circuit breaker and the event.
type ListenerEvent struct {
	CB    *Breaker
	Event BreakerEvent
}

type state int

const (
	open     state = iota
	halfopen state = iota
	closed   state = iota
)

var (
	defaultInitialBackOffInterval = 500 * time.Millisecond
	defaultBackoffMaxElapsedTime  = 0 * time.Second
	defaultErrorHistoryDepth      = 10
)

// Error codes returned by Call
var (
	ErrBreakerOpen    = errors.New("breaker open")
	ErrBreakerTimeout = errors.New("breaker time out")
)

// TripFunc is a function called by a Breaker's Fail() function and determines whether
// the breaker should trip. It will receive the Breaker as an argument and returns a
// boolean. By default, a Breaker has no TripFunc.
type TripFunc func(*Breaker) bool

// Breaker is the base of a circuit breaker. It maintains failure and success counters
// as well as the event subscribers.
type Breaker struct {
	// BackOff is the backoff policy that is used when determining if the breaker should
	// attempt to retry. A breaker created with NewBreaker will use an exponential backoff
	// policy by default.
	BackOff backoff.BackOff

	// ShouldTrip is a TripFunc that determines whether a Fail() call should trip the breaker.
	// A breaker created with NewBreaker will not have a ShouldTrip by default, and thus will
	// never automatically trip.
	ShouldTrip TripFunc

	// Clock is used for controlling time in tests.
	Clock clock.Clock

	_              [4]byte // pad to fix golang issue #599
	consecFailures int64
	lastFailure    int64 // stored as nanoseconds since the Unix epoch
	halfOpens      int64
	counts         *window
	nextBackOff    time.Duration
	tripped        int32
	broken         int32
	eventReceivers []chan BreakerEvent
	listeners      []chan ListenerEvent
	backoffLock    sync.Mutex

	//ring buffer for last N errors
	errorsBuffer *ring.Ring
	// errorsBufferLock used to prevent race accessing errorsBuffer (RWMutex is slower and atomic.Value same performance)
	errorsBufferLock sync.Mutex
}

// Options holds breaker configuration options.
type Options struct {
	BackOff           backoff.BackOff
	Clock             clock.Clock
	ShouldTrip        TripFunc
	WindowTime        time.Duration
	WindowBuckets     int
	ErrorHistoryDepth int
}

// NewBreakerWithOptions creates a base breaker with a specified backoff, clock and TripFunc
func NewBreakerWithOptions(options *Options) *Breaker {
	if options == nil {
		options = &Options{}
	}

	if options.Clock == nil {
		options.Clock = clock.New()
	}

	if options.BackOff == nil {
		b := backoff.NewExponentialBackOff()
		b.InitialInterval = defaultInitialBackOffInterval
		b.MaxElapsedTime = defaultBackoffMaxElapsedTime
		b.Clock = options.Clock
		b.Reset()
		options.BackOff = b
	}

	if options.WindowTime == 0 {
		options.WindowTime = DefaultWindowTime
	}

	if options.WindowBuckets == 0 {
		options.WindowBuckets = DefaultWindowBuckets
	}

	if options.ErrorHistoryDepth <= 0 {
		options.ErrorHistoryDepth = defaultErrorHistoryDepth
	}

	return &Breaker{
		BackOff:      options.BackOff,
		Clock:        options.Clock,
		ShouldTrip:   options.ShouldTrip,
		nextBackOff:  options.BackOff.NextBackOff(),
		counts:       newWindow(options.WindowTime, options.WindowBuckets),
		errorsBuffer: ring.New(options.ErrorHistoryDepth),
	}
}

// NewBreaker creates a base breaker with an exponential backoff and no TripFunc
func NewBreaker() *Breaker {
	return NewBreakerWithOptions(nil)
}

// NewThresholdBreaker creates a Breaker with a ThresholdTripFunc.
func NewThresholdBreaker(threshold int64) *Breaker {
	return NewBreakerWithOptions(&Options{
		ShouldTrip: ThresholdTripFunc(threshold),
	})
}

// NewConsecutiveBreaker creates a Breaker with a ConsecutiveTripFunc.
func NewConsecutiveBreaker(threshold int64) *Breaker {
	return NewBreakerWithOptions(&Options{
		ShouldTrip: ConsecutiveTripFunc(threshold),
	})
}

// NewRateBreaker creates a Breaker with a RateTripFunc.
func NewRateBreaker(rate float64, minSamples int64) *Breaker {
	return NewBreakerWithOptions(&Options{
		ShouldTrip: RateTripFunc(rate, minSamples),
	})
}

// Subscribe returns a channel of BreakerEvents. Whenever the breaker changes state,
// the state will be sent over the channel. See BreakerEvent for the types of events.
func (cb *Breaker) Subscribe() <-chan BreakerEvent {
	eventReader := make(chan BreakerEvent)
	output := make(chan BreakerEvent, 100)

	go func() {
		for v := range eventReader {
			select {
			case output <- v:
			default:
				<-output
				output <- v
			}
		}
	}()
	cb.eventReceivers = append(cb.eventReceivers, eventReader)
	return output
}

// AddListener adds a channel of ListenerEvents on behalf of a listener.
// The listener channel must be buffered.
func (cb *Breaker) AddListener(listener chan ListenerEvent) {
	cb.listeners = append(cb.listeners, listener)
}

// RemoveListener removes a channel previously added via AddListener.
// Once removed, the channel will no longer receive ListenerEvents.
// Returns true if the listener was found and removed.
func (cb *Breaker) RemoveListener(listener chan ListenerEvent) bool {
	for i, receiver := range cb.listeners {
		if listener == receiver {
			cb.listeners = append(cb.listeners[:i], cb.listeners[i+1:]...)
			return true
		}
	}
	return false
}

// Trip will trip the circuit breaker. After Trip() is called, Tripped() will
// return true.
func (cb *Breaker) Trip() {
	atomic.StoreInt32(&cb.tripped, 1)
	now := cb.Clock.Now()
	atomic.StoreInt64(&cb.lastFailure, now.UnixNano())
	cb.sendEvent(BreakerTripped)
}

// Reset will reset the circuit breaker. After Reset() is called, Tripped() will
// return false.
func (cb *Breaker) Reset() {
	atomic.StoreInt32(&cb.broken, 0)
	atomic.StoreInt32(&cb.tripped, 0)
	atomic.StoreInt64(&cb.halfOpens, 0)
	cb.ResetCounters()
	cb.sendEvent(BreakerReset)
}

// ResetCounters will reset only the failures, consecFailures, and success counters
func (cb *Breaker) ResetCounters() {
	atomic.StoreInt64(&cb.consecFailures, 0)
	cb.counts.Reset()
}

// Tripped returns true if the circuit breaker is tripped, false if it is reset.
func (cb *Breaker) Tripped() bool {
	return atomic.LoadInt32(&cb.tripped) == 1
}

// Break trips the circuit breaker and prevents it from auto resetting. Use this when
// manual control over the circuit breaker state is needed.
func (cb *Breaker) Break() {
	atomic.StoreInt32(&cb.broken, 1)
	cb.Trip()
}

// Failures returns the number of failures for this circuit breaker.
func (cb *Breaker) Failures() int64 {
	return cb.counts.Failures()
}

// ConsecFailures returns the number of consecutive failures that have occured.
func (cb *Breaker) ConsecFailures() int64 {
	return atomic.LoadInt64(&cb.consecFailures)
}

// Successes returns the number of successes for this circuit breaker.
func (cb *Breaker) Successes() int64 {
	return cb.counts.Successes()
}

// Fail is used to indicate a failure condition the Breaker should record. It will
// increment the failure counters and store the time of the last failure. If the
// breaker has a TripFunc it will be called, tripping the breaker if necessary.
func (cb *Breaker) Fail() {
	cb.counts.Fail()
	atomic.AddInt64(&cb.consecFailures, 1)
	now := cb.Clock.Now()
	atomic.StoreInt64(&cb.lastFailure, now.UnixNano())
	cb.sendEvent(BreakerFail)
	if cb.ShouldTrip != nil && cb.ShouldTrip(cb) {
		cb.Trip()
	}
}

// FailWithError is the same as Fail, but keeps history of errors in internal ring buffer
func (cb *Breaker) FailWithError(err error) {
	cb.errorsBufferLock.Lock()
	defer cb.errorsBufferLock.Unlock()

	cb.errorsBuffer = cb.errorsBuffer.Next()
	cb.errorsBuffer.Value = err
	cb.Fail()
}

// LastError returns last error from internal buffer
func (cb *Breaker) LastError() error {
	cb.errorsBufferLock.Lock()
	defer cb.errorsBufferLock.Unlock()

	if cb.errorsBuffer.Value == nil {
		return nil
	}
	return cb.errorsBuffer.Value.(error)
}

// Errors returns all errors from internal buffer
func (cb *Breaker) Errors() (errors []error) {
	cb.errorsBufferLock.Lock()
	defer cb.errorsBufferLock.Unlock()

	// reserve capacity to move last error to the end of slice without realloc
	errors = make([]error, 0, cb.errorsBuffer.Len()+1)
	cb.errorsBuffer.Do(func(x interface{}) {
		if x != nil {
			errors = append(errors, x.(error))
		}
	})
	// move last error to the end
	errors = append(errors[1:], errors[0])
	return errors
}

// Success is used to indicate a success condition the Breaker should record. If
// the success was triggered by a retry attempt, the breaker will be Reset().
func (cb *Breaker) Success() {
	cb.backoffLock.Lock()
	cb.BackOff.Reset()
	cb.nextBackOff = cb.BackOff.NextBackOff()
	cb.backoffLock.Unlock()

	state := cb.state()
	// if state was halfopen and it's successful request this state will be `open`.
	// due to cb.halfOpens is 1 at this point (request grouping)
	if state == halfopen || state == open {
		cb.Reset()
	}
	atomic.StoreInt64(&cb.consecFailures, 0)
	cb.counts.Success()
}

// ErrorRate returns the current error rate of the Breaker, expressed as a floating
// point number (e.g. 0.9 for 90%), since the last time the breaker was Reset.
func (cb *Breaker) ErrorRate() float64 {
	return cb.counts.ErrorRate()
}

// Ready will return true if the circuit breaker is ready to call the function.
// It will be ready if the breaker is in a reset state, or if it is time to retry
// the call for auto resetting.
func (cb *Breaker) Ready() bool {
	state := cb.state()
	if state == halfopen {
		atomic.StoreInt64(&cb.halfOpens, 0)
		cb.sendEvent(BreakerReady)
	}
	return state == closed || state == halfopen
}

// Call wraps a function the Breaker will protect. A failure is recorded
// whenever the function returns an error. If the called function takes longer
// than timeout to run, a failure will be recorded.
func (cb *Breaker) Call(circuit func() error, timeout time.Duration) error {
	return cb.CallContext(context.Background(), circuit, timeout)
}

// CallContext is same as Call but if the ctx is canceled after the circuit returned an error,
// the error will not be marked as a failure because the call was canceled intentionally.
func (cb *Breaker) CallContext(ctx context.Context, circuit func() error, timeout time.Duration) error {
	var err error

	if !cb.Ready() {
		return ErrBreakerOpen
	}

	if timeout == 0 {
		err = circuit()
	} else {
		c := make(chan error, 1)
		go func() {
			c <- circuit()
			close(c)
		}()

		select {
		case e := <-c:
			err = e
		case <-cb.Clock.After(timeout):
			err = ErrBreakerTimeout
		}
	}

	if err != nil {
		if ctx.Err() != context.Canceled {
			cb.FailWithError(err)
		}
		return err
	}

	cb.Success()
	return nil
}

// state returns the state of the TrippableBreaker. The states available are:
// closed - the circuit is in a reset state and is operational
// open - the circuit is in a tripped state
// halfopen - the circuit is in a tripped state but the reset timeout has passed
func (cb *Breaker) state() state {
	tripped := cb.Tripped()
	if tripped {
		if atomic.LoadInt32(&cb.broken) == 1 {
			return open
		}

		last := atomic.LoadInt64(&cb.lastFailure)
		since := cb.Clock.Now().Sub(time.Unix(0, last))

		cb.backoffLock.Lock()
		defer cb.backoffLock.Unlock()

		if cb.nextBackOff != backoff.Stop && since > cb.nextBackOff {
			if atomic.CompareAndSwapInt64(&cb.halfOpens, 0, 1) {
				cb.nextBackOff = cb.BackOff.NextBackOff()
				return halfopen
			}
			return open
		}
		return open
	}
	return closed
}

func (cb *Breaker) sendEvent(event BreakerEvent) {
	for _, receiver := range cb.eventReceivers {
		receiver <- event
	}
	for _, listener := range cb.listeners {
		le := ListenerEvent{CB: cb, Event: event}
		select {
		case listener <- le:
		default:
			<-listener
			listener <- le
		}
	}
}

// ThresholdTripFunc returns a TripFunc with that trips whenever
// the failure count meets the threshold.
func ThresholdTripFunc(threshold int64) TripFunc {
	return func(cb *Breaker) bool {
		return cb.Failures() == threshold
	}
}

// ConsecutiveTripFunc returns a TripFunc that trips whenever
// the consecutive failure count meets the threshold.
func ConsecutiveTripFunc(threshold int64) TripFunc {
	return func(cb *Breaker) bool {
		return cb.ConsecFailures() == threshold
	}
}

// RateTripFunc returns a TripFunc that trips whenever the
// error rate hits the threshold. The error rate is calculated as such:
// f = number of failures
// s = number of successes
// e = f / (f + s)
// The error rate is calculated over a sliding window of 10 seconds (by default)
// This TripFunc will not trip until there have been at least minSamples events.
func RateTripFunc(rate float64, minSamples int64) TripFunc {
	return func(cb *Breaker) bool {
		samples := cb.Failures() + cb.Successes()
		return samples >= minSamples && cb.ErrorRate() >= rate
	}
}
