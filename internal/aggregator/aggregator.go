package aggregator

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/geekeryy/docker-monitor/internal/model"
)

type Store interface {
	AppendBatch(ctx context.Context, batch model.LogBatch) error
}

type Aggregator struct {
	store         Store
	flushSize     int
	flushInterval time.Duration
	unknownLogID  string
	maxGroupSize  int

	mu     sync.Mutex
	groups map[string]*groupBuffer
}

type groupBuffer struct {
	firstSeen  time.Time
	lastSeen   time.Time
	containers map[string]struct{}
	hasAlert   bool
	events     []model.LogEvent
}

const maxBufferedEventsFactor = 10

func New(store Store, flushSize int, flushInterval time.Duration, unknownLogID string) *Aggregator {
	return &Aggregator{
		store:         store,
		flushSize:     flushSize,
		flushInterval: flushInterval,
		unknownLogID:  unknownLogID,
		maxGroupSize:  maxBufferedEvents(flushSize),
		groups:        make(map[string]*groupBuffer),
	}
}

func (a *Aggregator) Run(ctx context.Context) error {
	ticker := time.NewTicker(a.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := a.FlushAll(context.Background()); err != nil {
				return err
			}
		}
	}
}

func (a *Aggregator) Add(ctx context.Context, event model.LogEvent) error {
	if a.shouldFlushIndividually(event) {
		return a.store.AppendBatch(ctx, batchFromSingleEvent(event))
	}

	batch, shouldFlush := a.add(event)
	if !shouldFlush {
		return nil
	}
	return a.store.AppendBatch(ctx, batch)
}

func (a *Aggregator) shouldFlushIndividually(event model.LogEvent) bool {
	return event.AlertMatched && event.LogID == a.unknownLogID
}

func (a *Aggregator) FlushAll(ctx context.Context) error {
	batches := a.drainAll()
	var errs []error
	for _, batch := range batches {
		if err := a.store.AppendBatch(ctx, batch); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *Aggregator) add(event model.LogEvent) (model.LogBatch, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := event.LogID
	buffer, ok := a.groups[key]
	if !ok {
		buffer = &groupBuffer{
			firstSeen:  event.Timestamp,
			lastSeen:   event.Timestamp,
			containers: make(map[string]struct{}),
		}
		a.groups[key] = buffer
	}

	if buffer.firstSeen.IsZero() || event.Timestamp.Before(buffer.firstSeen) {
		buffer.firstSeen = event.Timestamp
	}
	if event.Timestamp.After(buffer.lastSeen) {
		buffer.lastSeen = event.Timestamp
	}

	buffer.events = append(buffer.events, event)
	if overflow := len(buffer.events) - a.maxGroupSize; overflow > 0 {
		buffer.events = append([]model.LogEvent(nil), buffer.events[overflow:]...)
		if len(buffer.events) > 0 {
			buffer.firstSeen = buffer.events[0].Timestamp
		}
	}
	buffer.containers[event.Container.Name] = struct{}{}
	buffer.hasAlert = buffer.hasAlert || event.AlertMatched

	if !buffer.hasAlert || len(buffer.events) < a.flushSize {
		return model.LogBatch{}, false
	}

	batch := snapshotBatch(key, buffer)
	delete(a.groups, key)
	return batch, true
}

func (a *Aggregator) drainAll() []model.LogBatch {
	a.mu.Lock()
	defer a.mu.Unlock()

	batches := make([]model.LogBatch, 0, len(a.groups))
	for key, buffer := range a.groups {
		if len(buffer.events) == 0 {
			continue
		}
		if !buffer.hasAlert {
			continue
		}
		batches = append(batches, snapshotBatch(key, buffer))
		delete(a.groups, key)
	}

	return batches
}

func snapshotBatch(key string, buffer *groupBuffer) model.LogBatch {
	containers := make([]string, 0, len(buffer.containers))
	for container := range buffer.containers {
		containers = append(containers, container)
	}

	events := append([]model.LogEvent(nil), buffer.events...)
	sort.SliceStable(events, func(i, j int) bool {
		return events[i].Timestamp.Before(events[j].Timestamp)
	})
	return model.LogBatch{
		LogID:      key,
		FirstSeen:  buffer.firstSeen,
		LastSeen:   buffer.lastSeen,
		Count:      len(events),
		Containers: containers,
		Events:     events,
		FlushedAt:  time.Now().UTC(),
	}
}

func batchFromSingleEvent(event model.LogEvent) model.LogBatch {
	return model.LogBatch{
		LogID:      event.LogID,
		FirstSeen:  event.Timestamp,
		LastSeen:   event.Timestamp,
		Count:      1,
		Containers: []string{event.Container.Name},
		Events:     []model.LogEvent{event},
		FlushedAt:  time.Now().UTC(),
	}
}

func maxBufferedEvents(flushSize int) int {
	if flushSize <= 0 {
		return maxBufferedEventsFactor
	}
	if flushSize > int(^uint(0)>>1)/maxBufferedEventsFactor {
		return flushSize
	}
	return flushSize * maxBufferedEventsFactor
}
