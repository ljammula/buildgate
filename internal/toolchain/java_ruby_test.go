package toolchain

import (
	"strings"
	"testing"
)

const pomWithRelease = `<?xml version="1.0"?>
<project xmlns="http://maven.apache.org/POM/4.0.0">
  <properties><maven.compiler.release>11</maven.compiler.release></properties>
</project>
`

const pomWithCompilerPluginRelease = `<project xmlns="http://maven.apache.org/POM/4.0.0">
  <properties><maven.compiler.release>${java.release}</maven.compiler.release></properties>
  <build><pluginManagement><plugins><plugin>
    <artifactId>maven-compiler-plugin</artifactId>
    <configuration><release>17</release></configuration>
  </plugin></plugins></pluginManagement></build>
</project>
`

// A pom.xml declares a JDK and Maven, a Gemfile Ruby and Bundler: where the
// language's version is read from, what to install, and whether these
// installed versions satisfy it.
func TestDetectJavaAndRuby(t *testing.T) {
	cases := []struct {
		name      string
		files     map[string]string
		tool      string
		source    string
		install   string
		satisfied []string
		refused   []string
	}{
		{"a pom with no pin takes any JDK and installs the default", map[string]string{"pom.xml": "<project/>"}, Java, "pom.xml", DefaultJava.String(),
			[]string{"8", "17.0.2", "25"}, nil},
		{".java-version is a minimum major", map[string]string{"pom.xml": pomWithRelease, ".java-version": "17\n"}, Java, ".java-version", "17",
			[]string{"17.0.0", "21.0.5"}, []string{"11.0.20", "8"}},
		{".java-version as jenv names it", map[string]string{"pom.xml": "<project/>", ".java-version": "temurin64-21.0.1\n"}, Java, ".java-version", "21",
			[]string{"21.0.5"}, []string{"17.0.9"}},
		{".java-version in the 1.x naming", map[string]string{"pom.xml": "<project/>", ".java-version": "1.8\n"}, Java, ".java-version", "8",
			[]string{"8", "11"}, nil},
		{".sdkmanrc when there is no .java-version", map[string]string{"pom.xml": pomWithRelease, ".sdkmanrc": "# sdk env\nmaven=3.9.9\njava=17.0.9-tem\n"}, Java, ".sdkmanrc", "17",
			[]string{"17.0.1"}, []string{"11.0.20"}},
		{"an alias in .java-version falls through to the pom", map[string]string{"pom.xml": pomWithRelease, ".java-version": "system\n"}, Java, "pom.xml", "11",
			[]string{"11.0.1", "21"}, []string{"8"}},
		{"maven.compiler.release", map[string]string{"pom.xml": pomWithRelease}, Java, "pom.xml", "11",
			[]string{"11.0.1"}, []string{"8"}},
		{"the compiler plugin's release when the property is a reference", map[string]string{"pom.xml": pomWithCompilerPluginRelease}, Java, "pom.xml", "17",
			[]string{"21"}, []string{"11.0.1"}},
		{"a Gemfile with no pin takes any Ruby and installs the default", map[string]string{"Gemfile": "source 'https://rubygems.org'\n"}, Ruby, "Gemfile", DefaultRuby.String(),
			[]string{"3.1.4", "4.0.1"}, nil},
		{".ruby-version with a ruby- prefix wins over the Gemfile", map[string]string{"Gemfile": "ruby '3.2.0'\n", ".ruby-version": "ruby-3.3\n"}, Ruby, ".ruby-version", "3.3",
			[]string{"3.3.0", "3.3.6"}, []string{"3.2.0", "3.4.1"}},
		{"the Gemfile's ruby line", map[string]string{"Gemfile": "source 'https://rubygems.org'\nruby \"3.2.2\"\n"}, Ruby, "Gemfile", "3.2.2",
			[]string{"3.2.2"}, []string{"3.2.3"}},
		{"another engine in .ruby-version falls through to the Gemfile", map[string]string{"Gemfile": "ruby '3.4'\n", ".ruby-version": "jruby-9.4.0\n"}, Ruby, "Gemfile", "3.4",
			[]string{"3.4.1"}, []string{"3.3.0"}},
		{"a ruby constraint is no pin", map[string]string{"Gemfile": "ruby '~> 3.3'\n"}, Ruby, "Gemfile", DefaultRuby.String(),
			[]string{"3.1.0"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reqs, err := Detect(repo(t, tc.files))
			if err != nil || len(reqs) != 2 {
				t.Fatalf("Detect = %+v, %v; want the language and its build tool", reqs, err)
			}
			lang, tool := reqs[0], reqs[1]
			if lang.Tool != tc.tool || lang.Source != tc.source || lang.Install.String() != tc.install {
				t.Errorf("requirement = %s from %s install %s, want %s from %s install %s", lang.Tool, lang.Source, lang.Install, tc.tool, tc.source, tc.install)
			}
			if tool.Install.String() != lang.Install.String() || (tool.Tool != Maven && tool.Tool != Bundler) {
				t.Errorf("build tool = %+v, want Maven or Bundler installed with %s", tool, lang.Install)
			}
			for _, have := range tc.satisfied {
				if f := Check(reqs, Installed{tc.tool: version(t, have)}); !f[0].OK {
					t.Errorf("%s %s does not satisfy %q", tc.tool, have, lang.Spec)
				}
			}
			for _, have := range tc.refused {
				if f := Check(reqs, Installed{tc.tool: version(t, have)}); f[0].OK {
					t.Errorf("%s %s satisfies %q", tc.tool, have, lang.Spec)
				}
			}
			if f := Check(reqs, Installed{}); f[0].OK || f[1].OK {
				t.Errorf("an image with neither tool satisfies %+v", f)
			}
		})
	}
}

func TestBuildToolIsSatisfiedByAnyVersionPresent(t *testing.T) {
	reqs, err := Detect(repo(t, map[string]string{"pom.xml": "<project/>", "Gemfile": ""}))
	if err != nil || len(reqs) != 4 {
		t.Fatalf("Detect = %+v, %v", reqs, err)
	}
	findings := Check(reqs, Installed{Java: version(t, "21"), Maven: version(t, "3.9.9"), Ruby: version(t, "3.3.0")})
	if !findings[0].OK || !findings[1].OK || !findings[2].OK || findings[3].Tool != Bundler || findings[3].OK {
		t.Errorf("findings = %+v, want Java, Maven and Ruby satisfied and Bundler missing", findings)
	}
}

func TestJavaMajorAndVersion(t *testing.T) {
	for in, want := range map[string]int{"17": 17, "1.8": 8, "1.8.0_432": 8, "21.0.1-tem": 21, "openjdk64-11.0.2": 11, "corretto-17": 17} {
		if got, ok := JavaMajor(in); !ok || got != want {
			t.Errorf("JavaMajor(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	if _, ok := JavaMajor("system"); ok {
		t.Error("JavaMajor read a version from an alias")
	}
	if got := JavaVersion(version(t, "1.8.0")).String(); got != "8.0" {
		t.Errorf("JavaVersion(1.8.0) = %s, want 8.0", got)
	}
	if got := JavaVersion(version(t, "21.0.5")).String(); got != "21.0.5" {
		t.Errorf("JavaVersion(21.0.5) = %s", got)
	}
}

func TestDetectJavaNamesAPomItCannotRead(t *testing.T) {
	if _, err := Detect(repo(t, map[string]string{"pom.xml": "<project><properties>"})); err == nil || !strings.Contains(err.Error(), "pom.xml does not parse") {
		t.Errorf("Detect = %v, want an error naming pom.xml", err)
	}
	// A pin read before the pom leaves the pom unread.
	if reqs, err := Detect(repo(t, map[string]string{"pom.xml": "<project><properties>", ".java-version": "17\n"})); err != nil || len(reqs) != 2 || reqs[0].Install.String() != "17" {
		t.Errorf("Detect = %+v, %v; want the .java-version pin", reqs, err)
	}
}
