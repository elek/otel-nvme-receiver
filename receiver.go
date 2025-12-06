package nvmereceiver

import (
	"context"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.uber.org/zap"
)

type SmartctlReceiver struct {
	consumer consumer.Metrics
	cancel   context.CancelFunc
	config   *Config
	logger   *zap.Logger
	producer *Producer
}

func (s *SmartctlReceiver) Start(ctx context.Context, host component.Host) error {
	ctx = context.Background()
	ctx, s.cancel = context.WithCancel(ctx)

	if s.config.TemperatureUnit == "" {
		s.config.TemperatureUnit = "celsius"
	}
	if s.config.Interval == "" {
		s.config.Interval = "60s"
	}

	collector := newNvmeCollector(s.logger, &s.config.TemperatureUnit)

	registry := prometheus.NewRegistry()

	err := registry.Register(collector)
	if err != nil {
		s.logger.Error("failed to register node collector", zap.Error(err))
		return nil
	}

	s.producer = NewMetricProducer(WithGatherer(registry), WithScope("nvme_exporter"))

	interval, _ := time.ParseDuration(s.config.Interval)
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				s.refresh(ctx)
			case <-ctx.Done():
				return
			}
		}
	}()
	return nil
}

func (s *SmartctlReceiver) Shutdown(ctx context.Context) error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *SmartctlReceiver) refresh(ctx context.Context) {
	metrics, err := s.producer.Produce(ctx)
	if err != nil {
		s.logger.Error("failed to gather metrics", zap.Error(err))
	}

	err = s.consumer.ConsumeMetrics(ctx, metrics)
	if err != nil {
		s.logger.Error("Failed to consume metrics", zap.Error(err))
	}
}
