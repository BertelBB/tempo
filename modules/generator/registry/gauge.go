package registry

import (
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"go.uber.org/atomic"
)

// this is mostly copied from counter

type gauge struct {
	metricName string

	// seriesMtx is used to sync modifications to the map, not to the data in series
	seriesMtx sync.RWMutex
	series    map[uint64]*gaugeSeries

	onAddSeries    func(count uint32) bool
	onRemoveSeries func(count uint32)
}

type gaugeSeries struct {
	// labelValueCombo should not be modified after creation
	labels      LabelPair
	value       *atomic.Float64
	lastUpdated *atomic.Int64
}

var _ Gauge = (*gauge)(nil)
var _ metric = (*gauge)(nil)

const add = "add"
const set = "set"

func newGauge(name string, onAddSeries func(uint32) bool, onRemoveSeries func(count uint32)) *gauge {
	if onAddSeries == nil {
		onAddSeries = func(uint32) bool {
			return true
		}
	}
	if onRemoveSeries == nil {
		onRemoveSeries = func(uint32) {}
	}

	return &gauge{
		metricName:     name,
		series:         make(map[uint64]*gaugeSeries),
		onAddSeries:    onAddSeries,
		onRemoveSeries: onRemoveSeries,
	}
}

func (g *gauge) Set(labelValueCombo *LabelValueCombo, value float64) {
	g.updateSeries(labelValueCombo, value, set)
}

func (g *gauge) Inc(labelValueCombo *LabelValueCombo, value float64) {
	g.updateSeries(labelValueCombo, value, add)
}

func (g *gauge) updateSeries(labelValueCombo *LabelValueCombo, value float64, operation string) {

	hash := labelValueCombo.getHash()

	g.seriesMtx.RLock()
	s, ok := g.series[hash]
	g.seriesMtx.RUnlock()

	if ok {
		g.updateSeriesValue(s, value, operation)
		return
	}

	if !g.onAddSeries(1) {
		return
	}

	newSeries := g.newSeries(labelValueCombo, value)

	g.seriesMtx.Lock()
	defer g.seriesMtx.Unlock()

	s, ok = g.series[hash]
	if ok {
		g.updateSeriesValue(s, value, operation)
		return
	}
	g.series[hash] = newSeries
}

func (g *gauge) newSeries(labelValueCombo *LabelValueCombo, value float64) *gaugeSeries {
	return &gaugeSeries{
		labels:      labelValueCombo.getLabelPair(),
		value:       atomic.NewFloat64(value),
		lastUpdated: atomic.NewInt64(time.Now().UnixMilli()),
	}
}

func (g *gauge) updateSeriesValue(s *gaugeSeries, value float64, operation string) {
	if operation == add {
		s.value.Add(value)
	} else {
		s.value = atomic.NewFloat64(value)
	}
	s.lastUpdated.Store(time.Now().UnixMilli())
}

func (g *gauge) name() string {
	return g.metricName
}

func (g *gauge) collectMetrics(appender storage.Appender, timeMs int64, externalLabels map[string]string) (activeSeries int, err error) {
	g.seriesMtx.RLock()
	defer g.seriesMtx.RUnlock()

	activeSeries = len(g.series)

	labelsCount := 0
	if activeSeries > 0 && g.series[0] != nil {
		labelsCount = len(g.series[0].labels.names)
	}
	lbls := make(labels.Labels, 1+len(externalLabels)+labelsCount)
	lb := labels.NewBuilder(lbls)

	// set metric name
	lb.Set(labels.MetricName, g.metricName)
	// set external labels
	for name, value := range externalLabels {
		lb.Set(name, value)
	}

	for _, s := range g.series {
		t := time.UnixMilli(timeMs)
		// set series-specific labels
		for i, name := range s.labels.names {
			lb.Set(name, s.labels.values[i])
		}
		_, err = appender.Append(0, lb.Labels(nil), t.UnixMilli(), s.value.Load())
		if err != nil {
			return
		}

		// TODO support exemplars
	}

	return
}

func (g *gauge) removeStaleSeries(staleTimeMs int64) {
	g.seriesMtx.Lock()
	defer g.seriesMtx.Unlock()

	for hash, s := range g.series {
		if s.lastUpdated.Load() < staleTimeMs {
			delete(g.series, hash)
			g.onRemoveSeries(1)
		}
	}
}
