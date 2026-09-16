package imageref

import "testing"

// TestRegistry is the union of the two tables this package replaced, plus the
// cases that motivated moving to Docker's own parser (#355).
func TestRegistry(t *testing.T) {
	cases := map[string]string{
		// Unqualified: Docker Hub short names.
		"busybox":        DockerHub,
		"ubuntu":         DockerHub,
		"busybox:latest": DockerHub,

		// A first component that is not host-shaped is a Hub namespace, not a
		// registry. This is what makes an allowlist actually block Hub rather
		// than treating "library" as a registry.
		"library/ubuntu":  DockerHub,
		"library/busybox": DockerHub,
		"myuser/myimage":  DockerHub,

		// Explicit hosts.
		"docker.io/library/ubuntu":       DockerHub,
		"ghcr.io/o/r":                    "ghcr.io",
		"contoso.azurecr.io/app":         "contoso.azurecr.io",
		"contoso.azurecr.io/team/app:v1": "contoso.azurecr.io",
		"registry.example.com:5/a/b":     "registry.example.com:5",

		// localhost is a host despite having no dot — Docker's special case.
		"localhost/x":        "localhost",
		"localhost/app":      "localhost",
		"localhost:5000/x":   "localhost:5000",
		"localhost:5000/app": "localhost:5000",

		// A port makes a bare name host-shaped.
		"registry:5000/app": "registry:5000",
	}
	for image, want := range cases {
		got, ok := Registry(image)
		if !ok {
			t.Errorf("Registry(%q) could not be parsed, want %q", image, want)
			continue
		}
		if got != want {
			t.Errorf("Registry(%q) = %q, want %q", image, got, want)
		}
	}
}

// TestUppercaseFirstComponentIsADomain is the #355 parity gap.
//
// Docker's splitDockerDomain treats a first component that is not all-lowercase
// as a domain, because a Hub namespace may not contain uppercase. The old
// hand-parser resolved MYREG/img to docker.io, so an allowlist that permitted
// Docker Hub also permitted a pull from a single-label host called MYREG — the
// gate judging one registry while the daemon contacted another.
func TestUppercaseFirstComponentIsADomain(t *testing.T) {
	for image, want := range map[string]string{
		"MYREG/img":             "MYREG",
		"MyReg/img":             "MyReg",
		"UPPER/lower":           "UPPER",
		"MYREG.example.com/img": "MYREG.example.com",
	} {
		got, ok := Registry(image)
		if !ok {
			t.Errorf("Registry(%q) could not be parsed, want %q", image, want)
			continue
		}
		if got != want {
			t.Errorf("Registry(%q) = %q, want %q — an uppercase first component is a domain", image, got, want)
		}
	}
}

// TestDockerHubSpellings pins which Hub aliases are folded and which are not.
//
// #355 asked for BOTH index.docker.io and registry-1.docker.io to canonicalise
// to docker.io. Only the first one does, and matching Docker exactly is the
// point — folding registry-1.docker.io ourselves would reintroduce a
// gate/daemon disagreement in the other direction.
func TestDockerHubSpellings(t *testing.T) {
	if got, ok := Registry("index.docker.io/library/x"); !ok || got != DockerHub {
		t.Errorf("Registry(index.docker.io/library/x) = %q,%v; want %q — Docker canonicalises this one", got, ok, DockerHub)
	}
	if got, ok := Registry("registry-1.docker.io/library/x"); !ok || got != "registry-1.docker.io" {
		t.Errorf("Registry(registry-1.docker.io/library/x) = %q,%v; want %q — Docker does NOT fold this one, so neither do we",
			got, ok, "registry-1.docker.io")
	}
}

// TestUnparseableIsNotDockerHub is the fail-safe property.
//
// An unparseable reference must report ok == false rather than quietly becoming
// docker.io. A caller that collapses it into Docker Hub would permit it
// whenever Hub is on the allowlist, for a reference the daemon would then
// reject or resolve somewhere else.
func TestUnparseableIsNotDockerHub(t *testing.T) {
	for _, image := range []string{
		"",
		"   ",
		"\t\n",
		"/leading",
		"host./img",
		"user@host/img",
		"UPPER/UPPER", // uppercase PATH is invalid, unlike an uppercase domain
	} {
		if got, ok := Registry(image); ok {
			t.Errorf("Registry(%q) = %q, ok=true; want ok=false", image, got)
		}
	}
}

// TestRegistryTrimsWhitespace: a value out of a registry key or a YAML file can
// carry padding, and ParseNormalizedNamed rejects it. Trimming is a parsing
// concern, so it happens here rather than being pushed onto every caller.
func TestRegistryTrimsWhitespace(t *testing.T) {
	for _, in := range []string{" ghcr.io/o/r", "ghcr.io/o/r ", "\tghcr.io/o/r\n"} {
		got, ok := Registry(in)
		if !ok || got != "ghcr.io" {
			t.Errorf("Registry(%q) = %q,%v; want %q,true", in, got, ok, "ghcr.io")
		}
	}
}
