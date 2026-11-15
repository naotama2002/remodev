package store

import "testing"

func TestPersistAndResume(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Create(&Session{ID: "a1", Status: StatusIdle}); err != nil {
		t.Fatal(err)
	}
	ch, cancel := s.Subscribe("a1")
	defer cancel()
	for i := 0; i < 3; i++ {
		if _, err := s.Append("a1", Event{Type: "user", Text: "hi"}); err != nil {
			t.Fatal(err)
		}
	}
	if ev := <-ch; ev.Seq != 1 {
		t.Fatalf("seq=%d", ev.Seq)
	}
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	evs, _ := s2.Events("a1", 1)
	if len(evs) != 2 || evs[0].Seq != 2 {
		t.Fatalf("events after reopen: %+v", evs)
	}
	ev, _ := s2.Append("a1", Event{Type: "x"})
	if ev.Seq != 4 {
		t.Fatalf("seq continues: %d", ev.Seq)
	}
}
