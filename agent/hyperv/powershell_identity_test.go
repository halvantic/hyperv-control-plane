package hyperv

import "testing"

func TestOuFromDN(t *testing.T) {
	cases := []struct {
		name string
		dn   string
		want string
	}{
		{"nested OU", "CN=HV01,OU=BallastHosts,OU=Hyper-V,DC=ballast,DC=local", "OU=BallastHosts,OU=Hyper-V,DC=ballast,DC=local"},
		{"single OU", "CN=HV01,OU=BallastHosts,DC=ballast,DC=local", "OU=BallastHosts,DC=ballast,DC=local"},
		{"default Computers container", "CN=HV01,CN=Computers,DC=ballast,DC=local", "CN=Computers,DC=ballast,DC=local"},
		{"no comma at all", "CN=HV01", ""},
		{"trailing comma only", "CN=HV01,", ""},
		{"empty string", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ouFromDN(c.dn); got != c.want {
				t.Errorf("ouFromDN(%q) = %q, want %q", c.dn, got, c.want)
			}
		})
	}
}
