/*
 *  Copyright 2021 Couchbase, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file  except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the  License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package fluent

import (
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/couchbase/fluent-bit/pkg/common"
	"github.com/couchbase/fluent-bit/pkg/logging"
	"github.com/fsnotify/fsnotify"
	"github.com/oklog/run"
)

var (
	log = logging.Log
)

const (
	maxDelayTime  = time.Minute * 5
	resetTime     = time.Minute * 10
	backoffFactor = 2
)

type Config struct {
	cmd                        *exec.Cmd
	mutex                      sync.Mutex
	restartTimes               int
	timer                      *time.Timer
	binPath, cfgPath, watchDir string
	totalStarts                int
	cleanStop                  bool
	cleanStart                 bool
}

func NewFluentBitConfig(binary, config, watchDir string) *Config {
	fb := Config{
		cmd:          nil,
		restartTimes: 0,
		mutex:        sync.Mutex{},
		timer:        time.NewTimer(0),
		binPath:      binary,
		cfgPath:      config,
		watchDir:     watchDir,
		totalStarts:  0,
		cleanStop:    false,
		cleanStart:   false,
	}

	return &fb
}

func (fb *Config) GetStartCount() int {
	fb.mutex.Lock()
	defer fb.mutex.Unlock()

	return fb.totalStarts
}

func (fb *Config) IsCleanStart() bool {
	fb.mutex.Lock()
	defer fb.mutex.Unlock()

	return fb.cleanStart
}

func Start(fb *Config) {
	if fb == nil {
		return
	}

	fb.mutex.Lock()
	defer fb.mutex.Unlock()

	if fb.cmd != nil {
		return
	}

	configContents, configErr := ioutil.ReadFile(fb.cfgPath)
	if configErr != nil {
		log.Errorw("Unable to retrieve Fluent bit config contents", "error", configErr, "config", fb.cfgPath)
	} else {
		log.Infow("Starting Fluent Bit", "binary", fb.binPath, "config", fb.cfgPath, "contents", string(configContents))
	}

	// #nosec G204
	fb.cmd = exec.Command(fb.binPath, "-c", fb.cfgPath)
	// Pick up any customised environment loaded in as well
	fb.cmd.Env = os.Environ()
	fb.cmd.Stdout = os.Stdout
	fb.cmd.Stderr = os.Stderr

	fb.totalStarts++
	fb.cleanStop = false
	fb.cleanStart = false

	if err := fb.cmd.Start(); err != nil {
		if configErr != nil {
			log.Errorw("Start Fluent bit error", "error", err, "binary", fb.binPath, "config", fb.cfgPath, "configError", configErr)
		} else {
			log.Errorw("Start Fluent bit error", "error", err, "binary", fb.binPath, "config", fb.cfgPath, "contents", string(configContents))
		}

		fb.cmd = nil

		return
	}

	fb.cleanStart = true
	log.Infow("Fluent bit started", "binary", fb.binPath, "config", fb.cfgPath)
}

func Wait(fb *Config) {
	if fb == nil || fb.cmd == nil {
		return
	}

	startTime := time.Now()

	// If killed by us this is normal
	if !fb.cleanStop {
		// If not killed by us then grab the config as well to check if that is the cause
		config, err := ioutil.ReadFile(fb.cfgPath)
		if err != nil {
			log.Errorw("Fluent bit exited", "error", fb.cmd.Wait(), "binary", fb.binPath, "config", fb.cfgPath, "configError", err)
		} else {
			log.Errorw("Fluent bit exited", "error", fb.cmd.Wait(), "binary", fb.binPath, "config", fb.cfgPath, "contents", string(config))
		}
	}
	// Once the fluent bit has executed for 10 minutes without any problems,
	// it should resets the restart backoff timer.
	if time.Since(startTime) >= resetTime {
		fb.restartTimes = 0
	}

	fb.mutex.Lock()
	fb.cmd = nil
	fb.mutex.Unlock()
}

func backoff(fb *Config) {
	if fb == nil {
		return
	}

	delayTime := time.Duration(math.Pow(backoffFactor, float64(fb.restartTimes))) * time.Second
	if delayTime >= maxDelayTime {
		delayTime = maxDelayTime
	}

	fb.timer.Reset(delayTime)

	startTime := time.Now()

	<-fb.timer.C

	log.Infow("Backing off with delay", "actual", time.Since(startTime), "expected", delayTime)

	fb.restartTimes++
}

func Stop(fb *Config) {
	if fb == nil {
		return
	}

	fb.mutex.Lock()
	defer fb.mutex.Unlock()

	if fb.cmd == nil || fb.cmd.Process == nil {
		return
	}

	fb.cleanStop = true

	if err := fb.cmd.Process.Kill(); err != nil {
		log.Errorw("Error killing Fluent Bit", "error", err)
	} else {
		log.Info("Killed Fluent Bit")
	}
}

func resetTimer(fb *Config) {
	if fb == nil {
		return
	}

	if fb.timer != nil {
		if !fb.timer.Stop() {
			<-fb.timer.C
		}

		fb.timer.Reset(0)
	}

	fb.restartTimes = 0
}

func addFluentBitWatcher(g *run.Group, config *Config) {
	// Watch the Fluent bit, if the Fluent bit not exists or stopped, restart it.
	cancel := make(chan struct{})

	g.Add(
		func() error {
			for {
				select {
				case <-cancel:
					return nil
				default:
				}

				// Start fluent bit if it does not exist.
				Start(config)
				// Wait for the fluent bit exit.
				Wait(config)
				log.Info("Detected exit of Fluent Bit so backoff")
				// After the fluent bit exit, fluent bit watcher restarts it with an exponential
				// back-off delay (1s, 2s, 4s, ...), that is capped at five minutes.
				backoff(config)
			}
		},
		func(err error) {
			close(cancel)
			Stop(config)
			resetTimer(config)
		},
	)
}

func AddDynamicConfigWatcher(g *run.Group, fb *Config) error {
	// Watch the config file, if the config file changed, stop Fluent bit.
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("unable to create dynamic config watcher: %w", err)
	}

	// Start watcher.
	err = watcher.Add(fb.watchDir)
	if err != nil {
		return fmt.Errorf("unable to add %q to dynamic config watcher: %w", fb.watchDir, err)
	}

	cancel := make(chan struct{})

	g.Add(
		func() error {
			for {
				select {
				case <-cancel:
					return nil
				case event := <-watcher.Events:
					if !common.IsValidEvent(event) {
						continue
					}

					// After the config file changed, it should stop the fluent bit,
					// and resets the restart backoff timer.
					log.Info("Config file changed, stopping Fluent Bit")
					Stop(fb)
					resetTimer(fb)
				case <-watcher.Errors:
					log.Error("Dynamic config watcher stopped")

					return nil
				}
			}
		},
		func(err error) {
			_ = watcher.Close()
			close(cancel)
		},
	)

	addFluentBitWatcher(g, fb)

	log.Info("Added FB watchers")

	return nil
}
