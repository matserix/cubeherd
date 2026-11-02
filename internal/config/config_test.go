package config

import (
	"sync"
	"testing"
)

func TestGetReturnsACopyOfTheLists(t *testing.T) {
	st, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *Settings) {
		s.BindIPs = []string{"10.0.0.1"}
		s.Nodes = []Node{{ID: "a", PortMin: 1, BindIPs: []string{"10.0.0.2"}}}
	})
	got := st.Get()
	got.BindIPs[0] = "changed"
	got.Nodes[0].PortMin = 99
	got.Nodes[0].BindIPs[0] = "changed"
	again := st.Get()
	if again.BindIPs[0] != "10.0.0.1" || again.Nodes[0].PortMin != 1 || again.Nodes[0].BindIPs[0] != "10.0.0.2" {
		t.Fatalf("a caller changed the stored settings: %+v", again)
	}
}

// Readers and in-place updates at the same time. Meaningful under -race.
func TestConcurrentGetAndUpdate(t *testing.T) {
	st, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st.Update(func(s *Settings) { s.Nodes = []Node{{ID: "a", BindIPs: []string{"x"}}, {ID: "b"}} })
	done := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				for _, n := range st.Get().Nodes {
					_ = n.PortMin + len(n.BindIPs)
				}
			}
		}()
	}
	for i := 0; i < 200; i++ {
		st.Update(func(s *Settings) { s.Nodes[0].PortMin = i; s.Nodes[0].BindIPs = []string{"y", "z"} })
	}
	close(done)
	wg.Wait()
}
