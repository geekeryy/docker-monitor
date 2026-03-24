package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/geekeryy/docker-monitor/internal/aggregator"
	"github.com/geekeryy/docker-monitor/internal/config"
	dockermonitor "github.com/geekeryy/docker-monitor/internal/docker"
	"github.com/geekeryy/docker-monitor/internal/model"
	"github.com/geekeryy/docker-monitor/internal/parser"
	"github.com/geekeryy/docker-monitor/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "monitor failed: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	monitorStartedAt := time.Now().UTC()
	configPath := flag.String("f", "configs/config.yaml", "path to config file")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	sinceDuration, err := cfg.SinceDuration()
	if err != nil {
		return err
	}
	flushInterval, err := cfg.FlushIntervalDuration()
	if err != nil {
		return err
	}

	fileStore := store.NewFileStore(cfg.Storage.OutputDir)
	dingTalkStore := store.NewDingTalkStore(
		cfg.DingTalk.WebhookURL,
		cfg.DingTalk.Secret,
		cfg.DingTalk.AtAll,
		cfg.DingTalk.AtMobiles,
		cfg.DingTalk.MentionLevels,
	)
	outputStore := store.NewMultiStore(
		fileStore,
		store.NewBestEffortStore("dingtalk", dingTalkStore, logger),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hosts := cfg.Docker.HostsOrDefault()
	instances, err := newMonitorInstances(ctx, cfg, hosts, monitorStartedAt, sinceDuration, flushInterval, outputStore, logger)
	if err != nil {
		return err
	}
	defer func() {
		for _, instance := range instances {
			_ = instance.client.Close()
		}
	}()

	logger.Info("starting docker warn monitor",
		slog.Any("docker_hosts", hosts),
		slog.Any("patterns", cfg.Docker.IncludePatterns),
		slog.String("output_dir", cfg.Storage.OutputDir),
		slog.Bool("dingtalk_enabled", dingTalkStore != nil),
	)

	errCh := make(chan error, len(instances)*2)
	var wg sync.WaitGroup

	for _, instance := range instances {
		instance := instance
		wg.Add(2)

		go func() {
			defer wg.Done()
			if err := instance.aggregator.Run(ctx); err != nil && ctx.Err() == nil {
				errCh <- fmt.Errorf("docker host %s aggregator stopped: %w", instance.label, err)
				stop()
			}
		}()

		go func() {
			defer wg.Done()
			if err := instance.watcher.Run(ctx); err != nil {
				if ctx.Err() == nil {
					errCh <- fmt.Errorf("docker host %s watcher stopped: %w", instance.label, err)
					stop()
				}
				return
			}
			if ctx.Err() == nil {
				errCh <- fmt.Errorf("docker host %s watcher stopped unexpectedly", instance.label)
				stop()
			}
		}()
	}

	<-ctx.Done()
	wg.Wait()

	select {
	case err := <-errCh:
		return err
	default:
	}

	logger.Info("monitor stopped")
	return nil
}

type monitorInstance struct {
	label      string
	client     *dockermonitor.Client
	aggregator *aggregator.Aggregator
	watcher    *dockermonitor.Watcher
}

func newMonitorInstances(ctx context.Context, cfg config.Config, hosts []string, monitorStartedAt time.Time, sinceDuration, flushInterval time.Duration, outputStore *store.MultiStore, logger *slog.Logger) ([]monitorInstance, error) {
	instances := make([]monitorInstance, 0, len(hosts))
	multiHost := len(hosts) > 1

	for _, host := range hosts {
		client, err := newDockerClient(host)
		if err != nil {
			return nil, err
		}

		label := dockerHostLabel(host)
		instanceLogger := logger.With(slog.String("docker_host", label))
		selfContainerID, err := dockermonitor.DetectSelfContainerID(ctx, client)
		if err != nil {
			_ = client.Close()
			return nil, fmt.Errorf("detect self container on docker host %s: %w", label, err)
		}
		if selfContainerID != "" {
			instanceLogger.Info("auto excluding monitor container logs", slog.String("container_id", selfContainerID))
		}

		logParser, err := parser.New(cfg.Filters, cfg.Aggregation.UnknownLogID)
		if err != nil {
			_ = client.Close()
			return nil, err
		}

		logAggregator := aggregator.New(outputStore, cfg.Aggregation.FlushSize, flushInterval, cfg.Aggregation.UnknownLogID)
		logReader := dockermonitor.NewLogReader(client)
		watcher := dockermonitor.NewWatcher(client, logReader, cfg.Docker.IncludePatterns, selfContainerID, monitorStartedAt, sinceDuration, func(streamCtx context.Context, raw model.RawLog) error {
			if multiHost {
				raw.Container.Name = label + "/" + raw.Container.Name
			}

			event, ok, err := logParser.Parse(raw)
			if err != nil {
				instanceLogger.Warn("parse log failed",
					slog.String("container", raw.Container.Name),
					slog.String("error", err.Error()),
				)
				return nil
			}
			if !ok {
				return nil
			}
			return logAggregator.Add(streamCtx, *event)
		}, instanceLogger)

		instances = append(instances, monitorInstance{
			label:      label,
			client:     client,
			aggregator: logAggregator,
			watcher:    watcher,
		})
	}

	return instances, nil
}

func dockerHostLabel(host string) string {
	host = strings.TrimSpace(host)
	if host == "" || strings.HasPrefix(host, "unix://") {
		return "local"
	}
	if strings.HasPrefix(host, "ssh://") {
		trimmed := strings.TrimPrefix(host, "ssh://")
		if idx := strings.Index(trimmed, "/"); idx >= 0 {
			trimmed = trimmed[:idx]
		}
		trimmed = strings.TrimPrefix(trimmed, "@")
		if trimmed != "" {
			return trimmed
		}
		return "ssh"
	}
	if strings.HasPrefix(host, "tcp://") {
		return strings.TrimPrefix(host, "tcp://")
	}
	if strings.HasPrefix(host, "http://") {
		return strings.TrimPrefix(host, "http://")
	}
	if strings.HasPrefix(host, "https://") {
		return strings.TrimPrefix(host, "https://")
	}
	return host
}

func newDockerClient(host string) (*dockermonitor.Client, error) {
	client, err := dockermonitor.NewClient(host)
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	return client, nil
}
