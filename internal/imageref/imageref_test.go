package imageref

import "testing"

// TestRegistry is the union of the two tables this package replaced — the one
// in internal/wslc and the one in internal/policy. Each had cases the other
// lacked, which is its own argument for there being one of them.
func TestRegistry(t *testing.T) {
	cases := map[string]string{
		// Unqualified: Docker Hub short names.
		"busybox":        DockerHub,
		"ubuntu":         DockerHub,
		"busybox:latest": DockerHub,

		// A first component that is not host-shaped is a Hub namespace, not a
		// registry. This is the case that makes an allowlist actually block Hub
		// rather than treating "library" as a registry.
		"library/ubuntu":  DockerHub,
		"library/busybox": DockerHub,
		"myuser/myimage":  DockerHub,

		// Explicit hosts.
		"docker.io/library/ubuntu":       DockerHub,
		"ghcr.io/o/r":                    "ghcr.io",
		"contoso.azurecr.io/app":         "contoso.azurecr.io",
		"contoso.azurecr.io/team/app:v1": "contoso.azurecr.io",
		"registry.example.com:5/a/b":     "registry.example.com:5",

		// localhost is a host despite having no dot — the special case Docker
		// carves out.
		"localhost/x":        "localhost",
		"localhost/app":      "localhost",
		"localhost:5000/x":   "localhost:5000",
		"localhost:5000/app": "localhost:5000",

		// A port makes a bare name host-shaped.
		"registry:5000/app": "registry:5000",

		// Unknown, NOT Docker Hub. See the doc comment: the caller decides what
		// an unattributable reference means.
		"": "",
	}
	for image, want := range cases {
		if got := Registry(image); got != want {
			t.Errorf("Registry(%q) = %q, want %q", image, got, want)
		}
	}
}

// TestRegistryTrimsWhitespace pins the behaviour the two old copies disagreed
// on. wslc's trimmed; policy's did not and relied on its allowlist matcher to
// trim afterwards, which worked by luck rather than design — nothing downstream
// of an allowlist check is obliged to be forgiving.
func TestRegistryTrimsWhitespace(t *testing.T) {
	for _, in := range []string{" ghcr.io/o/r", "ghcr.io/o/r ", "\tghcr.io/o/r\n"} {
		if got := Registry(in); got != "ghcr.io" {
			t.Errorf("Registry(%q) = %q, want %q", in, got, "ghcr.io")
		}
	}
	for _, in := range []string{"   ", "\t", "\n"} {
		if got := Registry(in); got != "" {
			t.Errorf("Registry(%q) = %q, want \"\" (unknown)", in, got)
		}
	}
}
