package gocql

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

type eventDebouncer struct {
	name   string
	timer  *time.Timer
	mu     sync.Mutex
	events []frame

	callback func([]frame)
	quit     chan struct{}

	logger internalLogger
}

func newEventDebouncer(name string, eventHandler func([]frame), logger internalLogger) *eventDebouncer {
	e := &eventDebouncer{
		name:     name,
		quit:     make(chan struct{}),
		timer:    time.NewTimer(eventDebounceTime),
		callback: eventHandler,
		logger:   logger,
	}
	e.timer.Stop()
	go e.flusher()

	return e
}

func (e *eventDebouncer) stop() {
	e.quit <- struct{}{} // sync with flusher
	close(e.quit)
}

func (e *eventDebouncer) flusher() {
	for {
		select {
		case <-e.timer.C:
			e.mu.Lock()
			e.flush()
			e.mu.Unlock()
		case <-e.quit:
			return
		}
	}
}

const (
	eventBufferSize   = 1000
	eventDebounceTime = 1 * time.Second
)

// flush must be called with mu locked
func (e *eventDebouncer) flush() {
	if len(e.events) == 0 {
		return
	}

	// if the flush interval is faster than the callback then we will end up calling
	// the callback multiple times, probably a bad idea. In this case we could drop
	// frames?
	go e.callback(e.events)
	e.events = make([]frame, 0, eventBufferSize)
}

func (e *eventDebouncer) debounce(frame frame) {
	e.mu.Lock()
	e.timer.Reset(eventDebounceTime)

	// TODO: probably need a warning to track if this threshold is too low
	if len(e.events) < eventBufferSize {
		e.events = append(e.events, frame)
	} else {
		e.logger.Warning("%s: buffer full, dropping event frame: %s",
			NewLogField("event_name", e.name), NewLogField("frame", frame))
	}

	e.mu.Unlock()
}

func (s *Session) handleEvent(framer *framer) {
	frame, err := framer.parseFrame()
	if err != nil {
		s.logger.Error("unable to parse event frame: %v", NewLogField("err", err.Error()))
		return
	}

	s.logger.Debug("handling event frame: %v", NewLogField("frame", frame.String()))

	switch f := frame.(type) {
	case *schemaChangeKeyspace, *schemaChangeFunction,
		*schemaChangeTable, *schemaChangeAggregate, *schemaChangeType:

		s.schemaEvents.debounce(frame)
	case *topologyChangeEventFrame, *statusChangeEventFrame:
		s.nodeEvents.debounce(frame)
	default:
		s.logger.Error("invalid event frame (%v): %v",
			NewLogField("frame_type", fmt.Sprintf("%T", f)), NewLogField("frame", f.String()))
	}
}

func (s *Session) handleSchemaEvent(frames []frame) {
	// TODO: debounce events
	for _, frame := range frames {
		switch f := frame.(type) {
		case *schemaChangeKeyspace:
			s.schemaDescriber.clearSchema(f.keyspace)
			s.handleKeyspaceChange(f.keyspace, f.change)
		case *schemaChangeTable:
			s.schemaDescriber.clearSchema(f.keyspace)
		case *schemaChangeAggregate:
			s.schemaDescriber.clearSchema(f.keyspace)
		case *schemaChangeFunction:
			s.schemaDescriber.clearSchema(f.keyspace)
		case *schemaChangeType:
			s.schemaDescriber.clearSchema(f.keyspace)
		}
	}
}

func (s *Session) handleKeyspaceChange(keyspace, change string) {
	s.control.awaitSchemaAgreement()
	s.policy.KeyspaceChanged(KeyspaceUpdateEvent{Keyspace: keyspace, Change: change})
}

// handleNodeEvent handles inbound status and topology change events.
//
// Status events are debounced by host IP; only the latest event is processed.
//
// Topology events are debounced by performing a single full topology refresh
// whenever any topology event comes in.
//
// Processing topology change events before status change events ensures
// that a NEW_NODE event is not dropped in favor of a newer UP event (which
// would itself be dropped/ignored, as the node is not yet known).
func (s *Session) handleNodeEvent(frames []frame) {
	type nodeEvent struct {
		change string
		host   net.IP
		port   int
	}

	topologyEventReceived := false
	// status change events
	sEvents := make(map[string]*nodeEvent)

	for _, frame := range frames {
		switch f := frame.(type) {
		case *topologyChangeEventFrame:
			s.logger.Warning("received topology change event: %v",
				NewLogField("frame", strings.Join([]string{f.change, "->", f.host.String(), ":", strconv.Itoa(f.port)}, "")))
			topologyEventReceived = true
		case *statusChangeEventFrame:
			event, ok := sEvents[f.host.String()]
			if !ok {
				event = &nodeEvent{change: f.change, host: f.host, port: f.port}
				sEvents[f.host.String()] = event
			}
			event.change = f.change
		}
	}

	if topologyEventReceived && !s.cfg.Events.DisableTopologyEvents {
		s.debounceRingRefresh()
	}

	for _, f := range sEvents {
		s.logger.Info("dispatching status change event: %v",
			NewLogField("frame", strings.Join([]string{f.change, "->", f.host.String(), ":", strconv.Itoa(f.port)}, "")))

		// ignore events we received if they were disabled
		// see https://github.com/gocql/gocql/issues/1591
		switch f.change {
		case "UP":
			if !s.cfg.Events.DisableNodeStatusEvents {
				s.handleNodeUp(f.host, f.port)
			}
		case "DOWN":
			if !s.cfg.Events.DisableNodeStatusEvents {
				s.handleNodeDown(f.host, f.port)
			}
		}
	}
}

func (s *Session) handleNodeUp(eventIp net.IP, eventPort int) {
	s.logger.Info("node is UP: %s:%d",
		NewLogField("event_ip", eventIp.String()), NewLogField("event_port", eventPort))

	host, ok := s.ring.getHostByIP(eventIp.String())
	if !ok {
		s.debounceRingRefresh()
		return
	}

	if s.cfg.filterHost(host) {
		return
	}

	if d := host.Version().nodeUpDelay(); d > 0 {
		time.Sleep(d)
	}
	s.startPoolFill(host)
}

func (s *Session) startPoolFill(host *HostInfo) {
	// we let the pool call handleNodeConnected to change the host state
	s.pool.addHost(host)
	s.policy.AddHost(host)
}

func (s *Session) handleNodeConnected(host *HostInfo) {
	s.logger.Debug("connected to node: %s:%d (%s)",
		NewLogField("host_addr", host.ConnectAddress()), NewLogField("port", host.Port()), NewLogField("host_id", host.HostID()))

	host.setState(NodeUp)

	if !s.cfg.filterHost(host) {
		s.policy.HostUp(host)
	}
}

func (s *Session) handleNodeDown(ip net.IP, port int) {
	s.logger.Warning("node is DOWN: %s:%d",
		NewLogField("host_addr", ip.String()), NewLogField("port", port))

	host, ok := s.ring.getHostByIP(ip.String())
	if ok {
		host.setState(NodeDown)
		if s.cfg.filterHost(host) {
			return
		}

		s.policy.HostDown(host)
		hostID := host.HostID()
		s.pool.removeHost(hostID)
	}
}
