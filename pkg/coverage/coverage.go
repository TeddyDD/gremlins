/*
 * Copyright 2022 The Gremlins Authors
 *
 *    Licensed under the Apache License, Version 2.0 (the "License");
 *    you may not use this file except in compliance with the License.
 *    You may obtain a copy of the License at
 *
 *        http://www.apache.org/licenses/LICENSE-2.0
 *
 *    Unless required by applicable law or agreed to in writing, software
 *    distributed under the License is distributed on an "AS IS" BASIS,
 *    WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *    See the License for the specific language governing permissions and
 *    limitations under the License.
 */

package coverage

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/tools/cover"

	"github.com/go-gremlins/gremlins/configuration"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/pkg/log"
)

// Result contains the Profile generated by the coverage and the time
// it took to generate the coverage report.
type Result struct {
	Profile Profile
	Elapsed time.Duration
}

// Coverage is responsible for executing a Go test with coverage via the Run() method,
// then parsing the result coverage report file.
type Coverage struct {
	cmdContext execContext
	workDir    string
	path       string
	fileName   string
	mod        gomodule.GoModule

	buildTags string
}

// Option for the Coverage initialization.
type Option func(c *Coverage) *Coverage

type execContext = func(name string, args ...string) *exec.Cmd

// New instantiates a Coverage element using exec.Command as execContext,
// actually running the command on the OS.
func New(workdir string, mod gomodule.GoModule, opts ...Option) *Coverage {
	return NewWithCmd(exec.Command, workdir, mod, opts...)
}

// NewWithCmd instantiates a Coverage element given a custom execContext.
func NewWithCmd(cmdContext execContext, workdir string, mod gomodule.GoModule, opts ...Option) *Coverage {
	buildTags := configuration.Get[string](configuration.UnleashTagsKey)

	c := &Coverage{
		cmdContext: cmdContext,
		workDir:    workdir,
		path:       "./...",
		fileName:   "coverage",
		mod:        mod,
		buildTags:  buildTags,
	}
	for _, opt := range opts {
		c = opt(c)
	}

	return c
}

// Run executes the coverage command and parses the results, returning a *Profile
// object.
// Before executing the coverage, it downloads the go modules in a separate step.
// This is done to avoid that the download phase impacts the execution time which
// is later used as timeout for the mutant testing execution.
func (c *Coverage) Run() (Result, error) {
	log.Infof("Gathering coverage... ")
	if err := c.downloadModules(); err != nil {
		return Result{}, fmt.Errorf("impossible to download modules: %w", err)
	}
	elapsed, err := c.executeCoverage()
	if err != nil {
		return Result{}, fmt.Errorf("impossible to executeCoverage coverage: %w", err)
	}
	log.Infof("done in %s\n", elapsed)
	profile, err := c.getProfile()
	if err != nil {
		return Result{}, fmt.Errorf("an error occurred while generating coverage profile: %w", err)
	}

	return Result{Profile: profile, Elapsed: elapsed}, nil
}

func (c *Coverage) getProfile() (Profile, error) {
	cf, err := os.Open(c.filePath())
	defer func(cf *os.File) {
		_ = cf.Close()
	}(cf)
	if err != nil {
		return nil, err
	}
	profile, err := c.parse(cf)
	if err != nil {
		return nil, err
	}

	return profile, nil
}

func (c *Coverage) filePath() string {
	return fmt.Sprintf("%v/%v", c.workDir, c.fileName)
}

func (c *Coverage) downloadModules() error {
	cmd := c.cmdContext("go", "mod", "download")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

func (c *Coverage) executeCoverage() (time.Duration, error) {
	args := []string{"test"}
	if c.buildTags != "" {
		args = append(args, "-tags", c.buildTags)
	}
	args = append(args, "-cover", "-coverprofile", c.filePath(), c.path)
	cmd := c.cmdContext("go", args...)
	cmd.Stderr = os.Stderr

	start := time.Now()
	if err := cmd.Run(); err != nil {
		return 0, err
	}

	return time.Since(start), nil
}

func (c *Coverage) parse(data io.Reader) (Profile, error) {
	profiles, err := cover.ParseProfilesFromReader(data)
	if err != nil {
		return nil, err
	}
	status := make(Profile)
	for _, p := range profiles {
		for _, b := range p.Blocks {
			if b.Count == 0 {
				continue
			}
			block := Block{
				StartLine: b.StartLine,
				StartCol:  b.StartCol,
				EndLine:   b.EndLine,
				EndCol:    b.EndCol,
			}
			fn := c.removeModuleFromPath(p)
			status[fn] = append(status[fn], block)
		}
	}

	return status, nil
}

func (c *Coverage) removeModuleFromPath(p *cover.Profile) string {
	path := strings.ReplaceAll(p.FileName, c.mod.Name+"/", "")
	path, _ = filepath.Rel(c.mod.PkgDir, path)

	return path
}
