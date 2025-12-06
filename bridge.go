// Copyright The OpenTelemetry Authors
// SPDX-License-Identifier: Apache-2.0

// This file is the refactored version of https://github.com/open-telemetry/opentelemetry-go-contrib/blob/bridges/prometheus/v0.63.0/bridges/prometheus/producer.go
package nvmereceiver

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"go.opentelemetry.io/collector/pdata/pcommon"
	"go.opentelemetry.io/collector/pdata/pmetric"
	"go.opentelemetry.io/otel"
)

const (
	traceIDLabel = "trace_id"
	spanIDLabel  = "span_id"
)

var (
	errUnsupportedType = errors.New("unsupported metric type")
	processStartTime   = time.Now()
)

type Producer struct {
	gatherers prometheus.Gatherers
	scope     string
}

// NewMetricProducer returns a metric.Producer that fetches metrics from
// Prometheus. This can be used to allow Prometheus instrumentation to be
// added to an OpenTelemetry export pipeline.
func NewMetricProducer(opts ...Option) *Producer {
	cfg := newConfig(opts...)
	return &Producer{
		gatherers: cfg.gatherers,
		scope:     cfg.scope,
	}
}

func (p *Producer) Produce(context.Context) (pmetric.Metrics, error) {
	metrics := pmetric.NewMetrics()
	resourceMetrics := metrics.ResourceMetrics().AppendEmpty()
	now := time.Now()
	var errs multierr
	for _, gatherer := range p.gatherers {
		promMetrics, err := gatherer.Gather()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		err = convertPrometheusMetricsInto(p.scope, promMetrics, resourceMetrics, now)
		if err != nil {
			errs = append(errs, err)
		}
	}
	if errs.errOrNil() != nil {
		otel.Handle(errs.errOrNil())
	}
	return metrics, nil
}

func convertPrometheusMetricsInto(scope string, promMetrics []*dto.MetricFamily, metrics pmetric.ResourceMetrics, now time.Time) error {
	var errs multierr
	for _, pm := range promMetrics {
		if pm.GetType() != dto.MetricType_GAUGE && pm.GetType() != dto.MetricType_COUNTER &&
			pm.GetType() != dto.MetricType_SUMMARY && pm.GetType() != dto.MetricType_HISTOGRAM {
			errs = append(errs, fmt.Errorf("%w: %v for metric %v", errUnsupportedType, pm.GetType(), pm.GetName()))
			continue
		}
		sm := metrics.ScopeMetrics().AppendEmpty()
		sm.Scope().SetName(scope)
		m := sm.Metrics().AppendEmpty()

		if len(pm.GetMetric()) == 0 {
			// This shouldn't ever happen
			continue
		}

		m.SetName(pm.GetName())
		m.SetDescription(pm.GetHelp())
		switch pm.GetType() {
		case dto.MetricType_GAUGE:
			convertGauge(pm.GetMetric(), m, now)
		case dto.MetricType_COUNTER:
			convertCounter(pm.GetMetric(), m, now)
		case dto.MetricType_SUMMARY:
			convertSummary(pm.GetMetric(), m, now)
		case dto.MetricType_HISTOGRAM:
			if isExponentialHistogram(pm.GetMetric()[0].GetHistogram()) {
				convertExponentialHistogram(pm.GetMetric(), m, now)
			} else {
				convertHistogram(pm.GetMetric(), m, now)
			}
		}
	}
	return errs.errOrNil()
}

func isExponentialHistogram(hist *dto.Histogram) bool {
	// The prometheus go client ensures at least one of these is non-zero
	// so it can be distinguished from a fixed-bucket histogram.
	// https://github.com/prometheus/client_golang/blob/7ac90362b02729a65109b33d172bafb65d7dab50/prometheus/histogram.go#L818
	return hist.GetZeroThreshold() > 0 ||
		hist.GetZeroCount() > 0 ||
		len(hist.GetPositiveSpan()) > 0 ||
		len(hist.GetNegativeSpan()) > 0
}

func convertGauge(metrics []*dto.Metric, pm pmetric.Metric, now time.Time) {
	gauge := pm.SetEmptyGauge()
	for _, m := range metrics {
		dp := gauge.DataPoints().AppendEmpty()

		dp.SetDoubleValue(m.GetGauge().GetValue())
		convertLabels(m.GetLabel(), dp.Attributes())

		if m.GetTimestampMs() != 0 {
			dp.SetTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(m.GetTimestampMs())))
		} else {
			dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		}
	}
}

func convertCounter(metrics []*dto.Metric, pm pmetric.Metric, now time.Time) {
	sum := pm.SetEmptySum()
	sum.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)
	sum.SetIsMonotonic(true)

	for _, m := range metrics {
		dp := sum.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(processStartTime))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		convertLabels(m.GetLabel(), dp.Attributes())
		dp.SetIntValue(int64(m.GetCounter().GetValue()))

		if ex := m.GetCounter().GetExemplar(); ex != nil {
			convertExemplar(ex, dp.Exemplars())
		}
		createdTs := m.GetCounter().GetCreatedTimestamp()
		if createdTs.IsValid() {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(createdTs.AsTime()))
		}
		if m.GetTimestampMs() != 0 {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(m.GetTimestampMs())))
		}
	}
}

func convertExponentialHistogram(metrics []*dto.Metric, pm pmetric.Metric, now time.Time) {
	hist := pm.SetEmptyExponentialHistogram()
	hist.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

	for _, m := range metrics {
		dp := hist.DataPoints().AppendEmpty()
		convertLabels(m.GetLabel(), dp.Attributes())
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(processStartTime))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetSum(m.GetHistogram().GetSampleSum())
		dp.SetCount(m.GetHistogram().GetSampleCount())
		dp.SetScale(m.GetHistogram().GetSchema())
		dp.SetZeroCount(m.GetHistogram().GetZeroCount())
		dp.SetZeroThreshold(m.GetHistogram().GetZeroThreshold())

		convertExponentialBuckets(
			m.GetHistogram().GetPositiveSpan(),
			m.GetHistogram().GetPositiveDelta(),
			dp.Positive())

		convertExponentialBuckets(
			m.GetHistogram().GetNegativeSpan(),
			m.GetHistogram().GetNegativeDelta(),
			dp.Negative())

		createdTs := m.GetCounter().GetCreatedTimestamp()
		if createdTs.IsValid() {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(createdTs.AsTime()))
		}
		if m.GetTimestampMs() != 0 {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(m.GetTimestampMs())))
		}
	}
}

func convertExponentialBuckets(bucketSpans []*dto.BucketSpan, deltas []int64, pbuckets pmetric.ExponentialHistogramDataPointBuckets) {
	if len(bucketSpans) == 0 {
		return
	}
	// Prometheus Native Histograms buckets are indexed by upper boundary
	// while Exponential Histograms are indexed by lower boundary, the result
	// being that the Offset fields are different-by-one.
	initialOffset := bucketSpans[0].GetOffset() - 1
	// We will have one bucket count for each delta, and zeros for the offsets
	// after the initial offset.
	lenCounts := len(deltas)
	for i, bs := range bucketSpans {
		if i != 0 {
			lenCounts += int(bs.GetOffset())
		}
	}
	counts := make([]uint64, lenCounts)
	deltaIndex := 0
	countIndex := int32(0)
	count := int64(0)
	for i, bs := range bucketSpans {
		// Do not insert zeroes if this is the first bucketSpan, since those
		// zeroes are accounted for in the Offset field.
		if i != 0 {
			// Increase the count index by the Offset to insert Offset zeroes
			countIndex += bs.GetOffset()
		}
		for range bs.GetLength() {
			// Convert deltas to the cumulative number of observations
			count += deltas[deltaIndex]
			deltaIndex++
			// count should always be positive after accounting for deltas
			if count > 0 {
				counts[countIndex] = uint64(count)
			}
			countIndex++
		}
	}
	pbuckets.SetOffset(initialOffset)
	for _, count := range counts {
		pbuckets.BucketCounts().Append(count)
	}
}

func convertHistogram(metrics []*dto.Metric, pm pmetric.Metric, now time.Time) {
	hist := pm.SetEmptyHistogram()
	hist.SetAggregationTemporality(pmetric.AggregationTemporalityCumulative)

	for _, m := range metrics {
		dp := hist.DataPoints().AppendEmpty()
		convertBuckets(m.GetHistogram().GetBucket(), m.GetHistogram().GetSampleCount(), dp)
		convertLabels(m.GetLabel(), dp.Attributes())
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(processStartTime))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetSum(m.GetHistogram().GetSampleSum())
		dp.SetCount(m.GetHistogram().GetSampleCount())

		createdTs := m.GetCounter().GetCreatedTimestamp()
		if createdTs.IsValid() {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(createdTs.AsTime()))
		}
		if m.GetTimestampMs() != 0 {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(m.GetTimestampMs())))
		}
	}
}

func convertBuckets(buckets []*dto.Bucket, sampleCount uint64, dp pmetric.HistogramDataPoint) {
	if len(buckets) == 0 {
		// This should never happen
		return
	}
	// buckets will only include the +Inf bucket if there is an exemplar for it
	// https://github.com/prometheus/client_golang/blob/d038ab96c0c7b9cd217a39072febd610bcdf1fd8/prometheus/metric.go#L189
	// we need to handle the case where it is present, or where it is missing.
	hasInf := math.IsInf(buckets[len(buckets)-1].GetUpperBound(), +1)

	var previousCount, bucketCount uint64
	for _, bucket := range buckets {
		// The last bound may be the +Inf bucket, which is implied in OTel, but
		// is explicit in Prometheus. Skip the last boundary if it is the +Inf
		// bound.
		if bound := bucket.GetUpperBound(); !math.IsInf(bound, +1) {
			dp.ExplicitBounds().Append(bound)
		}
		previousCount, bucketCount = bucket.GetCumulativeCount(), bucket.GetCumulativeCount()-previousCount
		dp.BucketCounts().Append(bucketCount)

		if ex := bucket.GetExemplar(); ex != nil {
			convertExemplar(ex, dp.Exemplars())
		}
	}
	if !hasInf {
		// The Inf bucket was missing, so set the last bucket counts to the
		// overall count
		dp.BucketCounts().Append(sampleCount - previousCount)
	}
}

func convertSummary(metrics []*dto.Metric, pm pmetric.Metric, now time.Time) {
	summary := pm.SetEmptySummary()
	for _, m := range metrics {
		dp := summary.DataPoints().AppendEmpty()
		dp.SetStartTimestamp(pcommon.NewTimestampFromTime(processStartTime))
		dp.SetTimestamp(pcommon.NewTimestampFromTime(now))
		dp.SetCount(m.GetSummary().GetSampleCount())
		dp.SetSum(m.GetSummary().GetSampleSum())
		convertLabels(m.GetLabel(), dp.Attributes())
		convertQuantiles(m.GetSummary().GetQuantile(), dp.QuantileValues())

		createdTs := m.GetSummary().GetCreatedTimestamp()
		if createdTs.IsValid() {
			dp.SetStartTimestamp(pcommon.NewTimestampFromTime(createdTs.AsTime()))
		}
		if t := m.GetTimestampMs(); t != 0 {
			dp.SetTimestamp(pcommon.NewTimestampFromTime(time.UnixMilli(t)))
		}
	}
}

func convertQuantiles(quantiles []*dto.Quantile, values pmetric.SummaryDataPointValueAtQuantileSlice) {
	for _, quantile := range quantiles {
		p := values.AppendEmpty()
		p.SetQuantile(quantile.GetQuantile())
		p.SetValue(quantile.GetValue())
	}

}

func convertLabels(labels []*dto.LabelPair, attributes pcommon.Map) {
	for _, l := range labels {
		attributes.PutStr(l.GetName(), l.GetValue())
	}
}

func convertExemplar(exemplar *dto.Exemplar, exemplars pmetric.ExemplarSlice) {
	ex := exemplars.AppendEmpty()
	var traceID pcommon.TraceID
	var spanID pcommon.SpanID
	// find the trace ID and span ID in attributes, if it exists
	for _, label := range exemplar.GetLabel() {
		switch label.GetName() {
		case traceIDLabel:
			copy(traceID[:], label.GetValue())
		case spanIDLabel:
			copy(spanID[:], label.GetValue())
		default:

			ex.FilteredAttributes().PutStr(label.GetName(), label.GetValue())
		}
	}

	ex.SetDoubleValue(exemplar.GetValue())
	ex.SetTimestamp(pcommon.NewTimestampFromTime(exemplar.GetTimestamp().AsTime()))
	ex.SetTraceID(traceID)
	ex.SetSpanID(spanID)
}

type multierr []error

func (e multierr) errOrNil() error {
	if len(e) == 0 {
		return nil
	} else if len(e) == 1 {
		return e[0]
	}
	return e
}

func (e multierr) Error() string {
	es := make([]string, len(e))
	for i, err := range e {
		es[i] = fmt.Sprintf("* %s", err)
	}
	return strings.Join(es, "\n\t")
}

// config contains options for the Producer.
type config struct {
	gatherers []prometheus.Gatherer
	scope     string
}

// newConfig creates a validated config configured with options.
func newConfig(opts ...Option) config {
	cfg := config{
		scope: "otelbridge",
	}
	for _, opt := range opts {
		cfg = opt.apply(cfg)
	}

	if len(cfg.gatherers) == 0 {
		cfg.gatherers = []prometheus.Gatherer{prometheus.DefaultGatherer}
	}

	return cfg
}

// Option sets producer option values.
type Option interface {
	apply(config) config
}

type optionFunc func(config) config

func (fn optionFunc) apply(cfg config) config {
	return fn(cfg)
}

// WithGatherer configures which prometheus Gatherer the Bridge will gather
// from. If no registerer is used the prometheus DefaultGatherer is used.
func WithGatherer(gatherer prometheus.Gatherer) Option {
	return optionFunc(func(cfg config) config {
		cfg.gatherers = append(cfg.gatherers, gatherer)
		return cfg
	})
}

func WithScope(scope string) Option {
	return optionFunc(func(cfg config) config {
		cfg.scope = scope
		return cfg
	})
}
