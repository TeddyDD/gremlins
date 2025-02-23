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

package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/go-gremlins/gremlins/cmd/internal/flags"
	"github.com/go-gremlins/gremlins/configuration"
	"github.com/go-gremlins/gremlins/internal/gomodule"
	"github.com/go-gremlins/gremlins/pkg/coverage"
	"github.com/go-gremlins/gremlins/pkg/log"
	"github.com/go-gremlins/gremlins/pkg/mutant"
	"github.com/go-gremlins/gremlins/pkg/mutator"
	"github.com/go-gremlins/gremlins/pkg/mutator/workdir"
	"github.com/go-gremlins/gremlins/pkg/report"
)

type unleashCmd struct {
	cmd *cobra.Command
}

const (
	commandName = "unleash"

	paramBuildTags = "tags"
	paramDryRun    = "dry-run"
	paramOutput    = "output"

	// Thresholds.
	paramThresholdEfficacy  = "threshold-efficacy"
	paramThresholdMCoverage = "threshold-mcover"
)

func newUnleashCmd(ctx context.Context) (*unleashCmd, error) {
	cmd := &cobra.Command{
		Use:     fmt.Sprintf("%s [path]", commandName),
		Aliases: []string{"run", "r"},
		Args:    cobra.MaximumNArgs(1),
		Short:   "Unleash the gremlins.",
		Long: `'gremlins unleash' unleashes the gremlins and performs mutation testing on 
a Go module. It works by first gathering the coverage of the test suite and then
analysing the source code to look for supported mutants.

Unleash only tests covered mutants, since it doesn't make sense to test mutants 
that no test case is able to catch.

In 'dry-run' mode, unleash only performs the analysis of the source code, but it
doesn't actually perform the test.

Thresholds are configurable quality gates that make gremlins exit with an error 
if those values are not met. Efficacy is the percent of KILLED mutants over
the total KILLED and LIVED mutants. Mutant coverage is the percent of total
KILLED + LIVED mutants, over the total mutants.`,
		RunE: runUnleash(ctx),
	}

	if err := setFlagsOnCmd(cmd); err != nil {
		return nil, err
	}

	return &unleashCmd{cmd: cmd}, nil
}

func runUnleash(ctx context.Context) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		log.Infoln("Starting...")
		currPath, runDir, err := changePath(args, os.Chdir, os.Getwd)
		if err != nil {
			return err
		}

		workDir, err := os.MkdirTemp(os.TempDir(), "gremlins-")
		if err != nil {
			return fmt.Errorf("impossible to create the workdir: %w", err)
		}
		defer cleanUp(workDir, runDir)

		wg := &sync.WaitGroup{}
		wg.Add(1)
		cancelled := false
		var results report.Results
		go runWithCancel(ctx, wg, func(c context.Context) {
			results, err = run(c, workDir, currPath)
		}, func() {
			cancelled = true
		})
		wg.Wait()
		if err != nil {
			return err
		}
		if cancelled {
			return nil
		}

		return report.Do(results)
	}
}

func runWithCancel(ctx context.Context, wg *sync.WaitGroup, runner func(c context.Context), onCancel func()) {
	c, cancel := context.WithCancel(ctx)
	go func() {
		<-ctx.Done()
		log.Infof("\nShutting down gracefully...\n")
		cancel()
		onCancel()
	}()
	runner(c)
	wg.Done()
}

func cleanUp(wd, rd string) {
	if err := os.Chdir(rd); err != nil {
		log.Errorf("impossible to move back to original folder: %s\n\t%s", err, wd)
	}
	if err := os.RemoveAll(wd); err != nil {
		log.Errorf("impossible to remove temporary folder: %s\n\t%s", err, wd)
	}
}

func run(ctx context.Context, workDir, currPath string) (report.Results, error) {
	mod, err := gomodule.Init(currPath)
	if err != nil {
		return report.Results{}, fmt.Errorf("%q is not in a Go module: %w", currPath, err)
	}
	c := coverage.New(workDir, mod)

	p, err := c.Run()
	if err != nil {
		return report.Results{}, fmt.Errorf("failed to gather coverage: %w", err)
	}

	d := workdir.NewDealer(workDir, mod.Root)

	mut := mutator.New(mod, p, d)
	results := mut.Run(ctx)

	return results, nil
}

func changePath(args []string, chdir func(dir string) error, getwd func() (string, error)) (string, string, error) {
	rd, err := getwd()
	if err != nil {
		return "", "", err
	}
	cp, _ := os.Getwd()
	if len(args) > 0 {
		cp, _ = filepath.Abs(args[0])
	}
	if cp != "." {
		err = chdir(cp)
		if err != nil {
			return "", "", err
		}
	}

	return cp, rd, nil
}

func setFlagsOnCmd(cmd *cobra.Command) error {
	cmd.Flags().SortFlags = false
	cmd.Flags().SetNormalizeFunc(func(f *pflag.FlagSet, name string) pflag.NormalizedName {
		from := []string{".", "_"}
		to := "-"
		for _, sep := range from {
			name = strings.ReplaceAll(name, sep, to)
		}

		return pflag.NormalizedName(name)
	})

	fls := []*flags.Flag{
		{Name: paramDryRun, CfgKey: configuration.UnleashDryRunKey, Shorthand: "d", DefaultV: false, Usage: "find mutations but do not executes tests"},
		{Name: paramBuildTags, CfgKey: configuration.UnleashTagsKey, Shorthand: "t", DefaultV: "", Usage: "a comma-separated list of build tags"},
		{Name: paramOutput, CfgKey: configuration.UnleashOutputKey, Shorthand: "o", DefaultV: "", Usage: "set the output file for machine readable results"},
		{Name: paramThresholdEfficacy, CfgKey: configuration.UnleashThresholdEfficacyKey, DefaultV: float64(0), Usage: "threshold for code-efficacy percent"},
		{Name: paramThresholdMCoverage, CfgKey: configuration.UnleashThresholdMCoverageKey, DefaultV: float64(0), Usage: "threshold for mutant-coverage percent"},
	}

	for _, f := range fls {
		err := flags.Set(cmd, f)
		if err != nil {
			return err
		}
	}

	return setMutantTypeFlags(cmd)
}

func setMutantTypeFlags(cmd *cobra.Command) error {
	for _, mt := range mutant.MutantTypes {
		name := mt.String()
		usage := fmt.Sprintf("enable %q mutants", name)
		param := strings.ReplaceAll(name, "_", "-")
		param = strings.ToLower(param)
		confKey := configuration.MutantTypeEnabledKey(mt)

		err := flags.Set(cmd, &flags.Flag{
			Name:     param,
			CfgKey:   confKey,
			DefaultV: configuration.IsDefaultEnabled(mt),
			Usage:    usage,
		})
		if err != nil {
			return err
		}
	}

	return nil
}
