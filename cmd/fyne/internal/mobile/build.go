// Copyright 2015 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:generate go run gendex.go -o dex.go

package mobile

import (
	"bufio"
	"flag"
	"fmt"
	"go/build"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var ctx = build.Default
var pkg *build.Package // TODO(crawshaw): remove global pkg variable
var tmpdir string

var cmdBuild = &command{
	run:   runBuild,
	Name:  "build",
	Usage: "[-target android|ios] [-o output] [-bundleid bundleID] [build flags] [package]",
	Short: "compile android APK and iOS app",
	Long: `
Build compiles and encodes the app named by the import path.

The named package must define a main function.

The -target Flag takes a target system name, either android (the
default) or ios.

For -target android, if an AndroidManifest.xml is defined in the
package directory, it is added to the APK output. Otherwise, a default
manifest is generated. By default, this builds a fat APK for all supported
instruction sets (arm, 386, amd64, arm64). A subset of instruction sets can
be selected by specifying target type with the architecture name. E.g.
-target=android/arm,android/386.

For -target ios, gomobile must be run on an OS X machine with Xcode
installed.

If the package directory contains an assets subdirectory, its contents
are copied into the output.

Flag -iosversion sets the minimal version of the iOS SDK to compile against.
The default version is 7.0.

Flag -androidapi sets the Android API version to compile against.
The default and minimum is 15.

The -bundleid Flag is required for -target ios and sets the bundle ID to use
with the app.

The -o Flag specifies the output file name. If not specified, the
output file name depends on the package built.

The -v Flag provides verbose output, including the list of packages built.

The build flags -a, -i, -n, -x, -gcflags, -ldflags, -tags, -trimpath, and -work are
shared with the build command. For documentation, see 'go help build'.
`,
}

const (
	minAndroidAPI = 15
)

func runBuild(cmd *command) (err error) {
	cleanup, err := buildEnvInit()
	if err != nil {
		return err
	}
	defer cleanup()

	args := cmd.Flag.Args()

	targetOS, targetArchs, err := parseBuildTarget(buildTarget)
	if err != nil {
		return fmt.Errorf(`invalid -target=%q: %v`, buildTarget, err)
	}

	oldCtx := ctx
	defer func() {
		ctx = oldCtx
	}()
	ctx.GOARCH = targetArchs[0]
	ctx.GOOS = targetOS

	if ctx.GOOS == "darwin" {
		ctx.BuildTags = append(ctx.BuildTags, "ios")
	}

	switch len(args) {
	case 0:
		pkg, err = ctx.ImportDir(cwd, build.ImportComment)
	case 1:
		pkg, err = ctx.Import(args[0], cwd, build.ImportComment)
	default:
		cmd.usage()
		os.Exit(1)
	}
	if err != nil {
		return err
	}

	if pkg.Name != "main" && buildO != "" {
		return fmt.Errorf("cannot set -o when building non-main package")
	}
	if buildBundleID == "" {
		return fmt.Errorf("value for -appID is required for a mobile package")
	}

	var nmpkgs map[string]bool
	switch targetOS {
	case "android":
		if pkg.Name != "main" {
			for _, arch := range targetArchs {
				env := androidEnv[arch]
				if err := goBuild(pkg.ImportPath, env); err != nil {
					return err
				}
			}
			return nil
		}
		nmpkgs, err = goAndroidBuild(pkg, buildBundleID, targetArchs, cmd.IconPath)
		if err != nil {
			return err
		}
	case "darwin":
		if !xcodeAvailable() {
			return fmt.Errorf("-target=ios requires XCode")
		}
		if pkg.Name != "main" {
			for _, arch := range targetArchs {
				env := darwinEnv[arch]
				if err := goBuild(pkg.ImportPath, env); err != nil {
					return err
				}
			}
			return nil
		}
		nmpkgs, err = goIOSBuild(pkg, buildBundleID, targetArchs)
		if err != nil {
			return err
		}
	}

	if !nmpkgs["github.com/fyne-io/mobile/app"] {
		return fmt.Errorf(`%s does not import "fyne.io/fyne/app"`, pkg.ImportPath)
	}

	return nil
}

var nmRE = regexp.MustCompile(`[0-9a-f]{8} t (?:.*/vendor/)?(github.com/fyne-io/mobile.*/[^.]*)`)

func extractPkgs(nm string, path string) (map[string]bool, error) {
	if buildN {
		return map[string]bool{"github.com/fyne-io/mobile/app": true}, nil
	}
	r, w := io.Pipe()
	cmd := exec.Command(nm, path)
	cmd.Stdout = w
	cmd.Stderr = os.Stderr

	nmpkgs := make(map[string]bool)
	errc := make(chan error, 1)
	go func() {
		s := bufio.NewScanner(r)
		for s.Scan() {
			if res := nmRE.FindStringSubmatch(s.Text()); res != nil {
				nmpkgs[res[1]] = true
			}
		}
		errc <- s.Err()
	}()

	err := cmd.Run()
	w.Close()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %v", nm, path, err)
	}
	if err := <-errc; err != nil {
		return nil, fmt.Errorf("%s %s: %v", nm, path, err)
	}
	return nmpkgs, nil
}

func importsApp(pkg *build.Package) error {
	// Building a program, make sure it is appropriate for mobile.
	for _, path := range pkg.Imports {
		if path == "github.com/fyne-io/mobile/app" {
			return nil
		}
	}
	return fmt.Errorf(`%s does not import "github.com/fyne-io/mobile/app"`, pkg.ImportPath)
}

var xout io.Writer = os.Stderr

func printcmd(format string, args ...interface{}) {
	cmd := fmt.Sprintf(format+"\n", args...)
	if tmpdir != "" {
		cmd = strings.Replace(cmd, tmpdir, "$WORK", -1)
	}
	if androidHome := os.Getenv("ANDROID_HOME"); androidHome != "" {
		cmd = strings.Replace(cmd, androidHome, "$ANDROID_HOME", -1)
	}
	if gomobilepath != "" {
		cmd = strings.Replace(cmd, gomobilepath, "$GOMOBILE", -1)
	}
	if gopath := goEnv("GOPATH"); gopath != "" {
		cmd = strings.Replace(cmd, gopath, "$GOPATH", -1)
	}
	if env := os.Getenv("HOMEPATH"); env != "" {
		cmd = strings.Replace(cmd, env, "$HOMEPATH", -1)
	}
	fmt.Fprint(xout, cmd)
}

// "Build flags", used by multiple commands.
var (
	buildA          bool   // -a
	buildI          bool   // -i
	buildN          bool   // -n
	buildV          bool   // -v
	buildX          bool   // -x
	buildO          string // -o
	buildGcflags    string // -gcflags
	buildLdflags    string // -ldflags
	buildTarget     string // -target
	buildTrimpath   bool   // -trimpath
	buildWork       bool   // -work
	buildBundleID   string // -bundleid
	buildIOSVersion string // -iosversion
	buildAndroidAPI int    // -androidapi
)

func RunNewBuild(target, appID, icon string) error {
	buildTarget = target
	buildBundleID = appID

	cmd := cmdBuild
	cmd.Flag = flag.FlagSet{}
	cmd.IconPath = icon
	return runBuild(cmd)
}

func addBuildFlags(cmd *command) {
	cmd.Flag.StringVar(&buildO, "o", "", "")
	cmd.Flag.StringVar(&buildGcflags, "gcflags", "", "")
	cmd.Flag.StringVar(&buildLdflags, "ldflags", "", "")
	cmd.Flag.StringVar(&buildTarget, "target", "android", "")
	cmd.Flag.StringVar(&buildBundleID, "bundleid", "", "")
	cmd.Flag.StringVar(&buildIOSVersion, "iosversion", "7.0", "")
	cmd.Flag.IntVar(&buildAndroidAPI, "androidapi", minAndroidAPI, "")

	cmd.Flag.BoolVar(&buildA, "a", false, "")
	cmd.Flag.BoolVar(&buildI, "i", false, "")
	cmd.Flag.BoolVar(&buildTrimpath, "trimpath", false, "")
	cmd.Flag.Var((*stringsFlag)(&ctx.BuildTags), "tags", "")
}

func addBuildFlagsNVXWork(cmd *command) {
	cmd.Flag.BoolVar(&buildN, "n", false, "")
	cmd.Flag.BoolVar(&buildV, "v", false, "")
	cmd.Flag.BoolVar(&buildX, "x", false, "")
	cmd.Flag.BoolVar(&buildWork, "work", false, "")
}

type binInfo struct {
	hasPkgApp bool
	hasPkgAL  bool
}

func init() {
	addBuildFlags(cmdBuild)
	addBuildFlagsNVXWork(cmdBuild)

	addBuildFlagsNVXWork(cmdClean)
}

func goBuild(src string, env []string, args ...string) error {
	return goCmd("build", []string{src}, env, args...)
}

func goInstall(srcs []string, env []string, args ...string) error {
	return goCmd("install", srcs, env, args...)
}

func goCmd(subcmd string, srcs []string, env []string, args ...string) error {
	cmd := exec.Command(
		goBin(),
		subcmd,
	)
	if len(ctx.BuildTags) > 0 {
		cmd.Args = append(cmd.Args, "-tags", strings.Join(ctx.BuildTags, " "))
	}
	if buildV {
		cmd.Args = append(cmd.Args, "-v")
	}
	if subcmd != "install" && buildI {
		cmd.Args = append(cmd.Args, "-i")
	}
	if buildX {
		cmd.Args = append(cmd.Args, "-x")
	}
	if buildGcflags != "" {
		cmd.Args = append(cmd.Args, "-gcflags", buildGcflags)
	}
	if buildLdflags != "" {
		cmd.Args = append(cmd.Args, "-ldflags", buildLdflags)
	}
	if buildTrimpath {
		cmd.Args = append(cmd.Args, "-trimpath")
	}
	if buildWork {
		cmd.Args = append(cmd.Args, "-work")
	}
	cmd.Args = append(cmd.Args, args...)
	cmd.Args = append(cmd.Args, srcs...)
	cmd.Env = append([]string{}, env...)
	// gomobile does not support modules yet.
	cmd.Env = append(cmd.Env, "GO111MODULE=off")
	return runCmd(cmd)
}

func parseBuildTarget(buildTarget string) (os string, archs []string, _ error) {
	if buildTarget == "" {
		return "", nil, fmt.Errorf(`invalid target ""`)
	}

	all := false
	archNames := []string{}
	for i, p := range strings.Split(buildTarget, ",") {
		osarch := strings.SplitN(p, "/", 2) // len(osarch) > 0
		if osarch[0] != "android" && osarch[0] != "ios" {
			return "", nil, fmt.Errorf(`unsupported os`)
		}

		if i == 0 {
			os = osarch[0]
		}

		if os != osarch[0] {
			return "", nil, fmt.Errorf(`cannot target different OSes`)
		}

		if len(osarch) == 1 {
			all = true
		} else {
			archNames = append(archNames, osarch[1])
		}
	}

	// verify all archs are supported one while deduping.
	isSupported := func(arch string) bool {
		for _, a := range allArchs {
			if a == arch {
				return true
			}
		}
		return false
	}

	seen := map[string]bool{}
	for _, arch := range archNames {
		if _, ok := seen[arch]; ok {
			continue
		}
		if !isSupported(arch) {
			return "", nil, fmt.Errorf(`unsupported arch: %q`, arch)
		}

		seen[arch] = true
		archs = append(archs, arch)
	}

	targetOS := os
	if os == "ios" {
		targetOS = "darwin"
	}
	if all {
		return targetOS, allArchs, nil
	}
	return targetOS, archs, nil
}
