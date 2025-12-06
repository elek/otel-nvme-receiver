package nvmereceiver

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/receiver"
)

var (
	typeStr = component.MustNewType("nvme")
)

type Config struct {
	Interval        string `mapstructure:"interval"`
	TemperatureUnit string `mapstructure:"temperature_unit"`
}

func createDefaultConfig() component.Config {
	return &Config{}
}

func NewFactory() receiver.Factory {
	return receiver.NewFactory(
		typeStr,
		createDefaultConfig,
		receiver.WithMetrics(createMetrics, component.StabilityLevelAlpha))
}

func createMetrics(ctx context.Context, settings receiver.Settings, config component.Config, consumer consumer.Metrics) (receiver.Metrics, error) {
	return &SmartctlReceiver{
		config:   config.(*Config),
		consumer: consumer,
		logger:   settings.Logger,
	}, nil
}
