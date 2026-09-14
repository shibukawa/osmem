package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	image       = "opensearchproject/opensearch:2.19.0"
	dataPath    = "/usr/share/opensearch/data"
	tmpfsOption = "rw,size=1g,uid=1000,gid=1000"
)

type sample struct {
	startup      time.Duration
	runnerBefore float64
	runnerReady  float64
	containerMIB float64
}

func main() {
	trials := 5
	if len(os.Args) == 2 {
		n, err := strconv.Atoi(os.Args[1])
		if err != nil || n < 1 {
			fatalf("usage: go run . [positive-trial-count]")
		}
		trials = n
	} else if len(os.Args) > 2 {
		fatalf("usage: go run . [positive-trial-count]")
	}

	ctx := context.Background()
	results := make([]sample, 0, trials)
	fmt.Printf("image=%s trials=%d tmpfs=%s:%s\n", image, trials, dataPath, tmpfsOption)

	for i := 0; i < trials; i++ {
		result, err := runTrial(ctx)
		if err != nil {
			fatalf("trial %d: %v", i+1, err)
		}
		results = append(results, result)
		fmt.Printf(
			"trial=%d startup=%.0fms runner_before=%.1fMiB runner_ready=%.1fMiB runner_delta=%.1fMiB container_memory=%.1fMiB\n",
			i+1,
			float64(result.startup.Microseconds())/1000,
			result.runnerBefore,
			result.runnerReady,
			result.runnerReady-result.runnerBefore,
			result.containerMIB,
		)
	}

	var startup, before, ready, container float64
	minStartup, maxStartup := results[0].startup, results[0].startup
	for _, result := range results {
		startup += float64(result.startup.Microseconds()) / 1000
		before += result.runnerBefore
		ready += result.runnerReady
		container += result.containerMIB
		if result.startup < minStartup {
			minStartup = result.startup
		}
		if result.startup > maxStartup {
			maxStartup = result.startup
		}
	}
	count := float64(len(results))
	fmt.Printf(
		"mean_startup=%.0fms range=%.0f-%.0fms runner_before=%.1fMiB runner_ready=%.1fMiB runner_delta=%.1fMiB container_memory=%.1fMiB\n",
		startup/count,
		float64(minStartup.Microseconds())/1000,
		float64(maxStartup.Microseconds())/1000,
		before/count,
		ready/count,
		(ready-before)/count,
		container/count,
	)
}

func runTrial(ctx context.Context) (sample, error) {
	runnerBefore, err := processRSSMiB()
	if err != nil {
		return sample{}, fmt.Errorf("read runner RSS before startup: %w", err)
	}

	start := time.Now()
	container, err := testcontainers.Run(
		ctx,
		image,
		testcontainers.WithExposedPorts("9200/tcp"),
		testcontainers.WithEnv(map[string]string{
			"discovery.type":          "single-node",
			"DISABLE_SECURITY_PLUGIN": "true",
			"OPENSEARCH_JAVA_OPTS":    "-Xms512m -Xmx512m",
		}),
		testcontainers.WithTmpfs(map[string]string{
			dataPath: tmpfsOption,
		}),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/").WithPort("9200/tcp").WithStartupTimeout(2*time.Minute),
		),
	)
	if err != nil {
		return sample{}, fmt.Errorf("start container: %w", err)
	}
	defer func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			fmt.Fprintf(os.Stderr, "warning: terminate container: %v\n", err)
		}
	}()

	host, err := container.Host(ctx)
	if err != nil {
		return sample{}, fmt.Errorf("resolve container host: %w", err)
	}
	port, err := container.MappedPort(ctx, "9200/tcp")
	if err != nil {
		return sample{}, fmt.Errorf("resolve OpenSearch port: %w", err)
	}
	if err := createBenchmarkIndex(ctx, "http://"+host+":"+port.Port()); err != nil {
		return sample{}, err
	}
	elapsed := time.Since(start)

	tmpfs, err := inspectTmpfs(container.GetContainerID())
	if err != nil {
		return sample{}, fmt.Errorf("verify tmpfs mount: %w", err)
	}

	runnerReady, err := processRSSMiB()
	if err != nil {
		return sample{}, fmt.Errorf("read runner RSS when ready: %w", err)
	}
	containerMIB, err := containerMemoryMiB(container.GetContainerID())
	if err != nil {
		return sample{}, fmt.Errorf("read container memory: %w", err)
	}
	fmt.Printf("verified_tmpfs=%s\n", strings.TrimSpace(tmpfs))

	return sample{
		startup:      elapsed,
		runnerBefore: runnerBefore,
		runnerReady:  runnerReady,
		containerMIB: containerMIB,
	}, nil
}

func createBenchmarkIndex(ctx context.Context, endpoint string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint+"/benchmark", nil)
	if err != nil {
		return fmt.Errorf("build index request: %w", err)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("PUT /benchmark: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("PUT /benchmark returned %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

func processRSSMiB() (float64, error) {
	out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(os.Getpid())).Output()
	if err != nil {
		return 0, err
	}
	kib, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, err
	}
	return kib / 1024, nil
}

func containerMemoryMiB(id string) (float64, error) {
	out, err := exec.Command("docker", "stats", "--no-stream", "--format", "{{.MemUsage}}", id).Output()
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) == 0 {
		return 0, fmt.Errorf("docker stats returned no memory value")
	}
	return parseMemoryMiB(strings.TrimSpace(fields[0]))
}

func parseMemoryMiB(value string) (float64, error) {
	var amount float64
	var unit string
	for i, r := range value {
		if (r < '0' || r > '9') && r != '.' {
			var err error
			amount, err = strconv.ParseFloat(value[:i], 64)
			if err != nil {
				return 0, fmt.Errorf("unrecognized memory value %q: %w", value, err)
			}
			unit = value[i:]
			break
		}
	}
	if unit == "" {
		return 0, fmt.Errorf("unrecognized memory value %q", value)
	}
	switch unit {
	case "B":
		return amount / (1024 * 1024), nil
	case "kB", "KB":
		return amount / 1024, nil
	case "KiB":
		return amount / 1024, nil
	case "MB":
		return amount * 1_000_000 / (1024 * 1024), nil
	case "MiB":
		return amount, nil
	case "GB":
		return amount * 1_000_000_000 / (1024 * 1024), nil
	case "GiB":
		return amount * 1024, nil
	default:
		return 0, fmt.Errorf("unrecognized memory unit %q", unit)
	}
}

func inspectTmpfs(id string) (string, error) {
	out, err := exec.Command("docker", "inspect", "--format", "{{json .HostConfig.Tmpfs}}", id).Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(out))
	if !strings.Contains(value, dataPath) ||
		!strings.Contains(value, "size=1g") ||
		!strings.Contains(value, "uid=1000") ||
		!strings.Contains(value, "gid=1000") {
		return "", fmt.Errorf("expected %s tmpfs with size=1g,uid=1000,gid=1000, got %s", dataPath, value)
	}
	return value, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
