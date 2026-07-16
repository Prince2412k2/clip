package tailnet

import "testing"

// withStatus swaps the package status func for a fixture and restores it.
func withStatus(t *testing.T, st *tsStatus) {
	t.Helper()
	orig := status
	status = func() (*tsStatus, error) { return st, nil }
	t.Cleanup(func() { status = orig })
}

func fixture() *tsStatus {
	return &tsStatus{
		Self: &tsNode{HostName: "debian", DNSName: "debian.tailnet.ts.net.", TailscaleIPs: []string{"100.87.152.55"}, Online: false},
		Peer: map[string]*tsNode{
			"a": {HostName: "contab", DNSName: "contab.tailnet.ts.net.", TailscaleIPs: []string{"100.108.114.11"}, Online: true},
			"b": {HostName: "iphone", DNSName: "iphone.tailnet.ts.net.", TailscaleIPs: []string{"100.64.0.9"}, Online: false},
			// infra node with no IPv4 -> must be filtered out
			"c": {HostName: "funnel-ingress", DNSName: "funnel.tailnet.ts.net.", TailscaleIPs: []string{}, Online: true},
		},
	}
}

func TestPeersIncludesSelf(t *testing.T) {
	withStatus(t, fixture())
	peers, err := Peers()
	if err != nil {
		t.Fatal(err)
	}

	byName := map[string]Peer{}
	for _, p := range peers {
		byName[p.Name] = p
	}

	self, ok := byName["debian"]
	if !ok {
		t.Fatalf("self (debian) missing from Peers(); got %v", peers)
	}
	if self.IP != "100.87.152.55" {
		t.Errorf("self IP = %q, want 100.87.152.55", self.IP)
	}
	if !self.Online {
		t.Error("self should always be reported online")
	}
	if _, ok := byName["contab"]; !ok {
		t.Error("peer contab missing")
	}
	if _, ok := byName["iphone"]; !ok {
		t.Error("peer iphone missing")
	}
	if _, ok := byName["funnel-ingress"]; ok {
		t.Error("infra node without IPv4 should be filtered out")
	}
}

func TestResolveSelfAndPeer(t *testing.T) {
	withStatus(t, fixture())
	cases := map[string]string{
		"debian": "100.87.152.55:8787",  // self by hostname
		"contab": "100.108.114.11:8787", // peer by hostname
	}
	for name, want := range cases {
		got, err := Resolve(name, 8787)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if got != want {
			t.Errorf("Resolve(%q) = %q, want %q", name, got, want)
		}
	}

	if _, err := Resolve("nope", 8787); err == nil {
		t.Error("Resolve of unknown name should error")
	}
}
