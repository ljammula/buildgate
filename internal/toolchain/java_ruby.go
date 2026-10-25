package toolchain

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// detectJava reads a root pom.xml: it declares a JDK and Maven. The JDK's
// version is .java-version, else .sdkmanrc's java= line, else the pom's
// maven.compiler.release (or the compiler plugin's <release>) when it is a
// plain number; with none of them any JDK satisfies it. A JDK compiles every
// older --release target, so an image's Java satisfies a pin when its major
// is at least the pinned one, and the JDK installed for a pin is the oldest
// LTS at or above it (JavaInstallMajor). A value that does not read as a Java
// major (an alias such as "system", "0") pins nothing, and the next source is
// read.
func detectJava(dir string) ([]Requirement, error) {
	if _, err := os.Stat(filepath.Join(dir, "pom.xml")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	java, err := javaPin(dir)
	if err != nil {
		return nil, err
	}
	if java == nil {
		java = &Requirement{Tool: Java, Source: "pom.xml", Spec: "a JDK", Install: DefaultJava,
			satisfied: func(Version) bool { return true }}
	}
	maven := Requirement{Tool: Maven, Source: "pom.xml", Spec: "mvn", Install: java.Install,
		satisfied: func(Version) bool { return true }}
	return []Requirement{*java, maven}, nil
}

// javaPin is the first Java version pinned by .java-version, .sdkmanrc or
// pom.xml, nil when none pins one.
func javaPin(dir string) (*Requirement, error) {
	lines, err := readLines(filepath.Join(dir, ".java-version"))
	if err != nil {
		return nil, err
	}
	if line := firstLine(lines); line != "" {
		if major, ok := JavaMajor(line); ok {
			return javaRequirement(".java-version", line, major), nil
		}
	}
	lines, err = readLines(filepath.Join(dir, ".sdkmanrc"))
	if err != nil {
		return nil, err
	}
	for _, line := range lines {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "java=")
		if major, ok := JavaMajor(value); found && ok {
			return javaRequirement(".sdkmanrc", strings.TrimSpace(line), major), nil
		}
	}
	release, err := pomRelease(filepath.Join(dir, "pom.xml"))
	if err != nil || release == "" {
		return nil, err
	}
	major, ok := JavaMajor(release)
	if !ok {
		return nil, nil
	}
	return javaRequirement("pom.xml", "release "+release, major), nil
}

func javaRequirement(source, spec string, major int) *Requirement {
	install := Version{Parts: []int{JavaInstallMajor(major)}}
	return &Requirement{Tool: Java, Source: source, Spec: spec, Install: install,
		satisfied: func(installed Version) bool { return installed.part(0) >= major }}
}

// javaLTSMajors are the Java majors an official maven:<line>-eclipse-temurin
// image is published for at or below the newest LTS, oldest first. Temurin
// publishes no maven image for the non-LTS majors among them (9, 10, 12-16).
var javaLTSMajors = []int{8, 11, 17, 21, 25}

// JavaInstallMajor is the JDK major installed for a pin of major: the oldest
// LTS major at or above it, or major itself above the newest LTS. Any JDK at
// or above the pin satisfies it, so a pin with no published image of its own
// gets the next one that has.
func JavaInstallMajor(major int) int {
	for _, lts := range javaLTSMajors {
		if lts >= major {
			return lts
		}
	}
	return major
}

var javaVersionSegment = regexp.MustCompile(`^\d+(\.\d+)*$`)

// JavaMajor reads a Java version as jenv (.java-version: "17",
// "temurin64-17.0.2", "1.8") and SDKMAN! (.sdkmanrc: "21.0.1-tem") write it,
// or as `java -version` prints it ("1.8.0_432", "21.0.5"): the first
// dash-separated segment that is a dotted number, with the old "1.x" naming
// read as x. ok is false when no segment is one, or the one that is does not
// read as a major of 1 or more (a number too large for an int, "0"): the
// caller then treats the declaration as no pin, never as major 0.
func JavaMajor(s string) (major int, ok bool) {
	for _, segment := range strings.Split(strings.TrimSpace(s), "-") {
		segment, _, _ = strings.Cut(segment, "_")
		if !javaVersionSegment.MatchString(segment) {
			continue
		}
		v, parsed := ParseVersion(segment)
		if !parsed {
			return 0, false
		}
		v = JavaVersion(v)
		if v.part(0) < 1 {
			return 0, false
		}
		return v.part(0), true
	}
	return 0, false
}

// JavaVersion is v with the old "1.x" naming of Java 8 and older read as x,
// so that an installed "1.8.0" compares as 8.
func JavaVersion(v Version) Version {
	if v.part(0) == 1 && len(v.Parts) > 1 {
		return Version{Parts: v.Parts[1:]}
	}
	return v
}

var plainNumber = regexp.MustCompile(`^\d+$`)

type pomPlugin struct {
	ArtifactID    string `xml:"artifactId"`
	Configuration struct {
		Release string `xml:"release"`
	} `xml:"configuration"`
}

type pomFile struct {
	Properties struct {
		Release string `xml:"maven.compiler.release"`
	} `xml:"properties"`
	Build struct {
		Plugins          []pomPlugin `xml:"plugins>plugin"`
		PluginManagement struct {
			Plugins []pomPlugin `xml:"plugins>plugin"`
		} `xml:"pluginManagement"`
	} `xml:"build"`
}

// pomRelease is the pom's maven.compiler.release property, else the
// maven-compiler-plugin's <release>, when it is a plain number; "" when it
// is absent or a property reference.
func pomRelease(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var pom pomFile
	if err := xml.Unmarshal(data, &pom); err != nil {
		return "", fmt.Errorf("pom.xml does not parse: %w", err)
	}
	candidates := []string{pom.Properties.Release}
	for _, plugin := range append(pom.Build.Plugins, pom.Build.PluginManagement.Plugins...) {
		if plugin.ArtifactID == "maven-compiler-plugin" {
			candidates = append(candidates, plugin.Configuration.Release)
		}
	}
	for _, candidate := range candidates {
		if candidate = strings.TrimSpace(candidate); plainNumber.MatchString(candidate) {
			return candidate, nil
		}
	}
	return "", nil
}

// detectRuby reads a root Gemfile: it declares Ruby and Bundler. Ruby's
// version is .ruby-version (a "ruby-" prefix stripped), else the Gemfile's
// `ruby "x.y[.z]"` line; an image's Ruby satisfies it when it matches the
// numbers stated. With neither, any Ruby satisfies it.
func detectRuby(dir string) ([]Requirement, error) {
	gemfile, err := readLines(filepath.Join(dir, "Gemfile"))
	if err != nil || gemfile == nil {
		return nil, err
	}
	pin, err := rubyPin(dir, gemfile)
	if err != nil {
		return nil, err
	}
	ruby := Requirement{Tool: Ruby, Source: "Gemfile", Spec: "ruby", Install: DefaultRuby,
		satisfied: func(Version) bool { return true }}
	if pin != nil {
		ruby = *pin
	}
	bundler := Requirement{Tool: Bundler, Source: "Gemfile", Spec: "bundle", Install: ruby.Install,
		satisfied: func(Version) bool { return true }}
	return []Requirement{ruby, bundler}, nil
}

var gemfileRubyLine = regexp.MustCompile(`^\s*ruby\s*\(?\s*["'](\d+(?:\.\d+){0,2})["']`)

// rubyPin is the Ruby version .ruby-version or the Gemfile's ruby line pins,
// nil when neither pins a number (an engine such as "jruby-9.4", a
// constraint such as "~> 3.3", or `ruby file: ".ruby-version"` with no file).
func rubyPin(dir string, gemfile []string) (*Requirement, error) {
	lines, err := readLines(filepath.Join(dir, ".ruby-version"))
	if err != nil {
		return nil, err
	}
	if line := firstLine(lines); line != "" {
		if req := rubyRequirement(".ruby-version", line, strings.TrimPrefix(line, "ruby-")); req != nil {
			return req, nil
		}
	}
	for _, line := range gemfile {
		if m := gemfileRubyLine.FindStringSubmatch(line); m != nil {
			return rubyRequirement("Gemfile", strings.TrimSpace(line), m[1]), nil
		}
	}
	return nil, nil
}

func rubyRequirement(source, spec, version string) *Requirement {
	if !pinPattern.MatchString(version) || strings.HasPrefix(version, "v") {
		return nil
	}
	pinned, ok := ParseVersion(version)
	if !ok {
		return nil
	}
	return &Requirement{Tool: Ruby, Source: source, Spec: spec, Install: pinned,
		satisfied: func(installed Version) bool { return pinned.samePrefix(installed) }}
}

// firstLine is the first line that is neither blank nor a comment.
func firstLine(lines []string) string {
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}
