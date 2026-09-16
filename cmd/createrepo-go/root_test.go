package main

import "testing"

func TestValidateSigningFlags(t *testing.T) {
	cases := []struct {
		name                       string
		gpgKey, gpgKeyID           string
		signMetadata, signPackages bool
		wantErr                    bool
	}{
		{name: "no signing, no key", wantErr: false},
		{name: "key with sign-packages", gpgKeyID: "ABC", signPackages: true, wantErr: false},
		{name: "key with sign-metadata", gpgKey: "k.asc", signMetadata: true, wantErr: false},
		{name: "key but no sign flag", gpgKeyID: "ABC", wantErr: true},
		{name: "key file but no sign flag", gpgKey: "k.asc", wantErr: true},
		{name: "both key sources", gpgKey: "k.asc", gpgKeyID: "ABC", signPackages: true, wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gf = globalFlags{gpgKey: c.gpgKey, gpgKeyID: c.gpgKeyID, signMetadata: c.signMetadata, signPackages: c.signPackages}
			err := validateSigningFlags()
			if (err != nil) != c.wantErr {
				t.Errorf("validateSigningFlags() err = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
	gf = globalFlags{}
}

func TestResolveProfile(t *testing.T) {
	cases := []struct {
		name              string
		target            string
		comp, sig         string
		compSet, sigSet   bool
		wantComp, wantSig string
		wantErr           bool
	}{
		{name: "no target keeps defaults", target: "", comp: "gzip", sig: "v4", wantComp: "gzip", wantSig: "v4"},
		{name: "rhel8 -> gzip+v4", target: "rhel8", comp: "gzip", sig: "v4", wantComp: "gzip", wantSig: "v4"},
		{name: "rhel9 -> zstd+v4", target: "rhel9", comp: "gzip", sig: "v4", wantComp: "zstd", wantSig: "v4"},
		{name: "rhel10 -> zstd+openpgp", target: "rhel10", comp: "gzip", sig: "v4", wantComp: "zstd", wantSig: "openpgp"},
		{name: "alias el9", target: "el9", comp: "gzip", sig: "v4", wantComp: "zstd", wantSig: "v4"},
		{name: "alias almalinux8 case-insensitive", target: "AlmaLinux8", comp: "zstd", sig: "openpgp", wantComp: "gzip", wantSig: "v4"},
		{name: "explicit compression overrides profile", target: "rhel9", comp: "gzip", sig: "v4", compSet: true, wantComp: "gzip", wantSig: "v4"},
		{name: "explicit signature overrides profile", target: "rhel10", comp: "gzip", sig: "v4", sigSet: true, wantComp: "zstd", wantSig: "v4"},
		{name: "unknown target errors", target: "rhel7", comp: "gzip", sig: "v4", wantErr: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			comp, sig, err := resolveProfile(c.target, c.comp, c.sig, c.compSet, c.sigSet)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for target %q", c.target)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if comp != c.wantComp || sig != c.wantSig {
				t.Errorf("resolveProfile(%q) = (%s,%s), want (%s,%s)", c.target, comp, sig, c.wantComp, c.wantSig)
			}
		})
	}
}
