// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package objabi

import (
	"fmt"
	"os"
	"reflect"
	"strings"

	"internal/goexperiment"
)

// Experiment contains the toolchain experiments enabled for the
// current build.
//
// (This is not necessarily the set of experiments the compiler itself
// was built with.)
var Experiment goexperiment.Flags

// FramePointerEnabled enables the use of platform conventions for
// saving frame pointers.
//
// This used to be an experiment, but now it's always enabled on
// platforms that support it.
//
// Note: must agree with runtime.framepointer_enabled.
var FramePointerEnabled = GOARCH == "amd64" || GOARCH == "arm64"

func init() {
	// Start with the baseline configuration.
	Experiment = goexperiment.BaselineFlags

	// Pick up any changes to the baseline configuration from the
	// GOEXPERIMENT environment. This can be set at make.bash time
	// and overridden at build time.
	env := envOr("GOEXPERIMENT", defaultGOEXPERIMENT)

	if env != "" {
		// Create a map of known experiment names.
		names := make(map[string]reflect.Value)
		rv := reflect.ValueOf(&Experiment).Elem()
		rt := rv.Type()
		for i := 0; i < rt.NumField(); i++ {
			field := rv.Field(i)
			names[strings.ToLower(rt.Field(i).Name)] = field
		}

		// Parse names.
		for _, f := range strings.Split(env, ",") {
			if f == "" {
				continue
			}
			if f == "none" {
				// GOEXPERIMENT=none restores the baseline configuration.
				// (This is useful for overriding make.bash-time settings.)
				Experiment = goexperiment.BaselineFlags
				continue
			}
			val := true
			if strings.HasPrefix(f, "no") {
				f, val = f[2:], false
			}
			field, ok := names[f]
			if !ok {
				fmt.Printf("unknown experiment %s\n", f)
				os.Exit(2)
			}
			field.SetBool(val)
		}
	}

	// regabi is only supported on amd64.
	if GOARCH != "amd64" {
		Experiment.Regabi = false
		Experiment.RegabiWrappers = false
		Experiment.RegabiG = false
		Experiment.RegabiReflect = false
		Experiment.RegabiDefer = false
		Experiment.RegabiArgs = false
	}
	// Setting regabi sets working sub-experiments.
	if Experiment.Regabi {
		Experiment.RegabiWrappers = true
		Experiment.RegabiG = true
		Experiment.RegabiReflect = true
		Experiment.RegabiDefer = true
		// Not ready yet:
		//Experiment.RegabiArgs = true
	}
	// Check regabi dependencies.
	if Experiment.RegabiG && !Experiment.RegabiWrappers {
		panic("GOEXPERIMENT regabig requires regabiwrappers")
	}
	if Experiment.RegabiArgs && !(Experiment.RegabiWrappers && Experiment.RegabiG && Experiment.RegabiReflect && Experiment.RegabiDefer) {
		panic("GOEXPERIMENT regabiargs requires regabiwrappers,regabig,regabireflect,regabidefer")
	}
}

// expList returns the list of lower-cased experiment names for
// experiments that differ from base. base may be nil to indicate no
// experiments.
func expList(exp, base *goexperiment.Flags) []string {
	var list []string
	rv := reflect.ValueOf(exp).Elem()
	var rBase reflect.Value
	if base != nil {
		rBase = reflect.ValueOf(base).Elem()
	}
	rt := rv.Type()
	for i := 0; i < rt.NumField(); i++ {
		name := strings.ToLower(rt.Field(i).Name)
		val := rv.Field(i).Bool()
		baseVal := false
		if base != nil {
			baseVal = rBase.Field(i).Bool()
		}
		if val != baseVal {
			if val {
				list = append(list, name)
			} else {
				list = append(list, "no"+name)
			}
		}
	}
	return list
}

// GOEXPERIMENT is a comma-separated list of enabled or disabled
// experiments that differ from the baseline experiment configuration.
// GOEXPERIMENT is exactly what a user would set on the command line
// to get the set of enabled experiments.
func GOEXPERIMENT() string {
	return strings.Join(expList(&Experiment, &goexperiment.BaselineFlags), ",")
}

// EnabledExperiments returns a list of enabled experiments, as
// lower-cased experiment names.
func EnabledExperiments() []string {
	return expList(&Experiment, nil)
}
