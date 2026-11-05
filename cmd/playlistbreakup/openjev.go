package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type modelConfig struct{ URL, Model, Backend, Weights, Python, Cache string }

func openJevConfig() (modelConfig, error) {
	backend := env("OPENJEV_BACKEND", "")

	if backend == "" {
		if runtime.GOOS == "darwin" && runtime.GOARCH == "arm64" {
			backend = "mlx"
		} else {
			backend = "laya"
		}
	}

	if backend != "mlx" && backend != "laya" {
		return modelConfig{}, errors.New("OPENJEV_BACKEND must be mlx or laya")
	}

	if backend == "mlx" && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
		return modelConfig{}, errors.New("the OpenJev MLX backend requires Apple silicon")
	}

	model, weights, venv := "openjev-latest", env("OPENJEV_MLX_MODEL", "mlx-community/diffusiongemma-26B-A4B-it-4bit"), "openjev-venv"

	if backend == "laya" {
		model, weights, venv = "laya-1.0", env("OPENJEV_LAYA_MODEL", "convaiinnovations/laya-typed-decisions"), "openjev-laya-venv"
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return modelConfig{}, err
	}

	previous := filepath.Join(home, "code/src/github.com/kristakoch/playlist-breakup/.tools")
	localPython := filepath.Join(".tools", venv, "bin/python")
	python := localPython
	if _, err := os.Stat(localPython); err != nil {
		old := filepath.Join(previous, venv, "bin/python")
		if _, err = os.Stat(old); err == nil {
			python = old
		}
	}

	cache := filepath.Join(".tools", "huggingface")
	if _, err := os.Stat(cache); err != nil {
		old := filepath.Join(previous, "huggingface")
		if _, err = os.Stat(old); err == nil {
			cache = old
		}
	}

	return modelConfig{URL: env("OPENJEV_URL", "http://127.0.0.1:8089"), Model: env("OPENJEV_MODEL", model), Backend: backend, Weights: weights, Python: env("OPENJEV_PYTHON", python), Cache: env("HF_HOME", cache)}, nil
}

func setupOpenJev(ctx context.Context, c modelConfig) error {
	uv, err := exec.LookPath("uv")
	if err != nil {
		home, _ := os.UserHomeDir()
		uv = filepath.Join(home, "code/src/github.com/kristakoch/playlist-breakup/.tools/uv-0.13.7/uv")
		if _, err = os.Stat(uv); err != nil {
			return errors.New("install uv and Python 3.12, then run task setup")
		}
	}

	project, venv := "model_runtime", "openjev-venv"

	if c.Backend == "laya" {
		project, venv = "model_runtime_cpu", "openjev-laya-venv"
	}

	pythonPath, err := filepath.Abs(filepath.Join(".tools", venv))
	if err != nil {
		return err
	}

	cmd := exec.CommandContext(ctx, uv, "sync", "--project", project, "--python", "3.12")
	cmd.Env = append(os.Environ(), "UV_PROJECT_ENVIRONMENT="+pythonPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func ensureOpenJev(ctx context.Context, c modelConfig) (func(), error) {
	mode := env("OPENJEV_MODE", "managed")

	if mode != "managed" && mode != "external" {
		return nil, errors.New("OPENJEV_MODE must be managed or external")
	}

	u, err := url.Parse(c.URL)
	if err != nil {
		return nil, err
	}

	probe := func() bool {
		req, err := http.NewRequestWithContext(ctx, "GET", strings.TrimRight(c.URL, "/")+"/health", nil)
		if err != nil {
			return false
		}

		if key := os.Getenv("OPENJEV_API_KEY"); key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}

		client := http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		resp, err := client.Do(req)
		if err != nil {
			return false
		}

		defer resp.Body.Close()
		var result struct {
			Status string `json:"status"`
		}

		return resp.StatusCode == 200 && json.NewDecoder(resp.Body).Decode(&result) == nil && result.Status == "ok"
	}

	if probe() {
		return func() {}, nil
	}

	if mode == "external" {
		return nil, errors.New("external OpenJev is not available at OPENJEV_URL")
	}

	if conn, err := net.DialTimeout("tcp", u.Host, time.Second); err == nil {
		conn.Close()

		return nil, errors.New("OpenJev port is occupied but the service is not ready; wait for it or choose a different OPENJEV_URL")
	}

	if _, err = os.Stat(c.Python); err != nil {
		return nil, fmt.Errorf("OpenJev runtime not found at %s; run task setup", c.Python)
	}

	if u.Port() == "" {
		return nil, errors.New("OPENJEV_URL must include a port")
	}

	cmd := exec.Command(c.Python, "-u", "-m", "openjev")
	// The app owns this process and forwards shutdown once; Ctrl+C should not
	// also reach Uvicorn directly through the terminal process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	envs := []string{}

	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "OPENJEV_MODEL=") {
			envs = append(envs, v)
		}
	}

	cmd.Env = append(envs, "OPENJEV_BACKEND="+c.Backend, "OPENJEV_HOST="+u.Hostname(), "OPENJEV_PORT="+u.Port(), "HF_HOME="+c.Cache)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		return nil, err
	}

	done := make(chan error, 1)

	go func() { done <- cmd.Wait() }()

	cleanup := func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	fmt.Println("Starting local OpenJev; model initialization may take several minutes.")
	deadline := time.NewTimer(30 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case err := <-done:
			return nil, fmt.Errorf("OpenJev exited during startup: %v", err)
		case <-ctx.Done():
			cleanup()
			return nil, ctx.Err()
		case <-deadline.C:
			cleanup()
			return nil, errors.New("OpenJev did not become ready within 30 minutes")
		case <-ticker.C:
			if probe() {
				select {
				case err := <-done:
					return nil, fmt.Errorf("OpenJev exited during startup: %v", err)
				default:
				}

				return cleanup, nil
			}
		}
	}
}
