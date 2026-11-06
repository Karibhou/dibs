package main

import "testing"

func TestHubURLFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name, env, want string
	}{
		{name: "unset uses the default", env: "", want: defaultHubURL},
		{name: "whitespace-only uses the default", env: "  \n", want: defaultHubURL},
		{name: "bare origin is unchanged", env: "https://hub.example", want: "https://hub.example"},
		{name: "trailing slash is dropped", env: "https://hub.example/", want: "https://hub.example"},
		{name: "repeated trailing slashes and whitespace are dropped", env: " https://hub.example// \n", want: "https://hub.example"},
		{name: "path prefix keeps its interior slashes", env: "http://127.0.0.1:9999/hub/", want: "http://127.0.0.1:9999/hub"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HUB_URL", tc.env)
			if got := hubURLFromEnv(); got != tc.want {
				t.Fatalf("hubURLFromEnv() = %q, want %q", got, tc.want)
			}
		})
	}
}
