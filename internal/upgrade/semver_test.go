package upgrade

import "testing"

func TestSemanticVersionComparison(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha",
		"1.0.0-alpha.1",
		"1.0.0-alpha.beta",
		"1.0.0-beta",
		"1.0.0-beta.2",
		"1.0.0-beta.11",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.1.0",
		"2.0.0",
	}
	for i := 0; i < len(ordered)-1; i++ {
		left, err := parseSemVersion(ordered[i])
		if err != nil {
			t.Fatal(err)
		}
		right, err := parseSemVersion(ordered[i+1])
		if err != nil {
			t.Fatal(err)
		}
		if compareSemVersion(left, right) >= 0 {
			t.Fatalf("expected %s < %s", ordered[i], ordered[i+1])
		}
	}
	one, err := parseSemVersion("v1.2.3+build.1")
	if err != nil {
		t.Fatal(err)
	}
	two, err := parseSemVersion("1.2.3+build.2")
	if err != nil {
		t.Fatal(err)
	}
	if compareSemVersion(one, two) != 0 {
		t.Fatal("build metadata affected precedence")
	}
}

func TestSemanticVersionRejectsInvalidValues(t *testing.T) {
	for _, value := range []string{"", "dev", "1", "1.2", "1.2.3.4", "01.2.3", "1.02.3", "1.2.03", "1.2.3-", "1.2.3-alpha..1", "1.2.3-alpha_1", "1.2.3+", "1.2.3+build_1", "1.2.3+one+two"} {
		if _, err := parseSemVersion(value); err == nil {
			t.Errorf("parseSemVersion(%q) succeeded", value)
		}
	}
}
