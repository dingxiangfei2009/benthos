/*
Copyright (c) 2014 Ashley Jeffs

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jeffail/benthos/input"
	"github.com/jeffail/benthos/message"
	"github.com/jeffail/benthos/output"
	"github.com/jeffail/util"
	"github.com/jeffail/util/log"
	"github.com/jeffail/util/path"
)

//--------------------------------------------------------------------------------------------------

// Config - The benthos configuration struct.
type Config struct {
	Input       input.Config            `json:"input" yaml:"input"`
	Output      output.Config           `json:"output" yaml:"output"`
	Logger      log.LoggerConfig        `json:"logger" yaml:"logger"`
	Stats       log.StatsConfig         `json:"stats" yaml:"stats"`
	Riemann     log.RiemannClientConfig `json:"riemann" yaml:"riemann"`
	StatsServer log.StatsServerConfig   `json:"stats_server" yaml:"stats_server"`
}

// NewConfig - Returns a new configuration with default values.
func NewConfig() Config {
	return Config{
		Input:       input.NewConfig(),
		Output:      output.NewConfig(),
		Logger:      log.DefaultLoggerConfig(),
		Stats:       log.DefaultStatsConfig(),
		Riemann:     log.NewRiemannClientConfig(),
		StatsServer: log.DefaultStatsServerConfig(),
	}
}

//--------------------------------------------------------------------------------------------------

func main() {
	var (
		err       error
		closeChan = make(chan struct{})
	)

	config := NewConfig()

	// A list of default config paths to check for if not explicitly defined
	defaultPaths := []string{}

	/* If we manage to get the path of our executable then we want to try and find config files
	 * relative to that path, we always check from the parent folder since we assume benthos is
	 * stored within the bin folder.
	 */
	if executablePath, err := path.BinaryPath(); err == nil {
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config.yaml"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config", "benthos.yaml"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config.json"))
		defaultPaths = append(defaultPaths, filepath.Join(executablePath, "..", "config", "benthos.json"))
	}

	defaultPaths = append(defaultPaths, []string{
		filepath.Join(".", "benthos.yaml"),
		filepath.Join(".", "benthos.json"),
		"/etc/benthos.yaml",
		"/etc/benthos.json",
		"/etc/benthos/config.yaml",
		"/etc/benthos/config.json",
	}...)

	// Load configuration etc
	if !util.Bootstrap(&config, defaultPaths...) {
		return
	}

	// Logging and stats aggregation
	// Note: Only log to Stderr if our output is stdout
	var logger *log.Logger
	if config.Output.Type == "stdout" {
		logger = log.NewLogger(os.Stderr, config.Logger)
	} else {
		logger = log.NewLogger(os.Stdout, config.Logger)
	}
	stats := log.NewStats(config.Stats)

	if riemannClient, err := log.NewRiemannClient(config.Riemann); err == nil {
		logger.UseRiemann(riemannClient)
		stats.UseRiemann(riemannClient)

		defer riemannClient.Close()
	} else if err != log.ErrEmptyConfigAddress {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Riemann client error: %v\n", err))
		return
	}
	defer stats.Close()

	// DEBUG
	routerCloseChan := make(chan struct{})
	routerClosedChan := make(chan struct{})
	go func() {
		consumerChan := make(chan message.Type)

		in := input.Construct(config.Input)
		out := output.Construct(config.Output, consumerChan)

		running := true
		for running {
			select {
			case msg := <-in.ConsumerChan():
				consumerChan <- msg
				select {
				case err := <-out.ErrorChan():
					if err != nil {
						logger.Errorf("Failed to push out message: %v\n", err)
					}
				case <-time.After(time.Second * 10):
					logger.Errorf("Timed out waiting for output confirmation")
				}
			case _, running = <-routerCloseChan:
			}
		}

		in.CloseAsync()
		out.CloseAsync()

		if err := in.WaitForClose(time.Second * 5); err != nil {
			logger.Errorf("Error closing input: %v\n", err)
		}
		for err := range out.ErrorChan() {
			if err != nil {
				logger.Errorf("Failed to push out message: %v\n", err)
			}
		}
		if err := out.WaitForClose(time.Second * 5); err != nil {
			logger.Errorf("Error closing output: %v\n", err)
		}

		close(routerClosedChan)
	}()
	defer func() {
		close(routerCloseChan)
		<-routerClosedChan
	}()
	// DEBUG END

	// Internal Statistics HTTP API
	statsServer, err := log.NewStatsServer(config.StatsServer, logger, stats)
	if err != nil {
		fmt.Fprintln(os.Stderr, fmt.Sprintf("Stats error: %v\n", err))
		return
	}

	go func() {
		if statserr := statsServer.Listen(); statserr != nil {
			fmt.Fprintln(os.Stderr, fmt.Sprintf("Stats server listen error: %v\n", statserr))
		}
	}()

	fmt.Fprintf(os.Stderr, "Launching a benthos instance, use CTRL+C to close.\n\n")

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	select {
	case <-sigChan:
	case <-closeChan:
	}
}

//--------------------------------------------------------------------------------------------------
