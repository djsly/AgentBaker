package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/Azure/agentbaker/aks-node-controller/parser"
	aksnodeconfigv1 "github.com/Azure/agentbaker/pkg/proto/aksnodeconfig/v1"
	"gopkg.in/fsnotify.v1"
)

type App struct {
	// cmdRunner is a function that runs the given command.
	// the goal of this field is to make it easier to test the app by mocking the command runner.
	cmdRunner func(cmd *exec.Cmd) error
}

func cmdRunner(cmd *exec.Cmd) error {
	return cmd.Run()
}

type ProvisionFlags struct {
	ProvisionConfig string
}

func (a *App) Run(ctx context.Context, args []string) int {
	slog.Info("aks-node-controller started")
	err := a.run(ctx, args)
	exitCode := errToExitCode(err)
	if exitCode == 0 {
		slog.Info("aks-node-controller finished successfully")
	} else {
		slog.Error("aks-node-controller failed", "error", err)
	}
	return exitCode
}

func (a *App) run(ctx context.Context, args []string) error {
	if len(args) < 2 {
		return errors.New("missing command argument")
	}
	switch args[1] {
	case "provision":
		fs := flag.NewFlagSet("provision", flag.ContinueOnError)
		provisionConfig := fs.String("provision-config", "", "path to the provision config file")
		err := fs.Parse(args[2:])
		if err != nil {
			return fmt.Errorf("parse args: %w", err)
		}
		if provisionConfig == nil || *provisionConfig == "" {
			return errors.New("--provision-config is required")
		}
		return a.Provision(ctx, ProvisionFlags{ProvisionConfig: *provisionConfig})
	case "provision-wait":
		provisionOutput, err := a.ProvisionWait(ctx)
		fmt.Println(provisionOutput)
		slog.Info("provision-wait finished", "provisionOutput", provisionOutput)
		return err
	default:
		return fmt.Errorf("unknown command: %s", args[1])
	}
}

func (a *App) Provision(ctx context.Context, flags ProvisionFlags) error {
	inputJSON, err := os.ReadFile(flags.ProvisionConfig)
	if err != nil {
		return fmt.Errorf("open provision file %s: %w", flags.ProvisionConfig, err)
	}

	config := &aksnodeconfigv1.Configuration{}
	err = json.Unmarshal(inputJSON, config)
	if err != nil {
		return fmt.Errorf("unmarshal provision config: %w", err)
	}
	if config.Version != "v0" {
		return fmt.Errorf("unsupported version: %s", config.Version)
	}

	cmd, err := parser.BuildCSECmd(ctx, config)
	if err != nil {
		return fmt.Errorf("build CSE command: %w", err)
	}
	var stdoutBuf, stderrBuf bytes.Buffer
	cmd.Stdout = io.MultiWriter(os.Stdout, &stdoutBuf)
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderrBuf)
	err = a.cmdRunner(cmd)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	// Is it ok to log a single line? Is it too much?
	slog.Info("CSE finished", "exitCode", exitCode, "stdout", stdoutBuf.String(), "stderr", stderrBuf.String(), "error", err)
	return err
}

func (a *App) ProvisionWait(ctx context.Context) (string, error) {
	if _, err := os.Stat(provisionJSONFilePath); err == nil {
		data, err := os.ReadFile(provisionJSONFilePath)
		if err != nil {
			return "", fmt.Errorf("failed to read provision.json: %w", err)
		}
		return string(data), nil
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return "", fmt.Errorf("failed to create watcher: %w", err)
	}
	defer watcher.Close()

	// Watch the directory containing the provision complete file
	dir := filepath.Dir(provisionCompleteFilePath)
	err = os.MkdirAll(dir, 0755) // create the directory if it doesn't exist
	if err != nil {
		return "", fmt.Errorf("fialed to create directory %s: %w", dir, err)
	}
	if err = watcher.Add(dir); err != nil {
		return "", fmt.Errorf("failed to watch directory: %w", err)
	}

	for {
		select {
		case event := <-watcher.Events:
			if event.Op&fsnotify.Create == fsnotify.Create && event.Name == provisionCompleteFilePath {
				data, err := os.ReadFile(provisionJSONFilePath)
				if err != nil {
					return "", fmt.Errorf("failed to read provision.json: %w", err)
				}
				return string(data), nil
			}

		case err := <-watcher.Errors:
			return "", fmt.Errorf("error watching file: %w", err)
		case _ = <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

var _ ExitCoder = &exec.ExitError{}

type ExitCoder interface {
	error
	ExitCode() int
}

func errToExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr ExitCoder
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}