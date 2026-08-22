package telegram

import (
	"reflect"
	"testing"
)

func TestParseCommandBasic(t *testing.T) {
	cmd, args := parseCommand("/Add survival 25565 193.23.221.21:25565")
	if cmd != "/add" {
		t.Fatalf("cmd = %q", cmd)
	}
	want := []string{"survival", "25565", "193.23.221.21:25565"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestParseCommandQuotedReason(t *testing.T) {
	cmd, args := parseCommand(`/ban 1.2.3.4 "spam flood proxy"`)
	if cmd != "/ban" {
		t.Fatalf("cmd = %q", cmd)
	}
	want := []string{"1.2.3.4", "spam flood proxy"}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("args = %#v, want %#v", args, want)
	}
}

func TestParseCommandEmpty(t *testing.T) {
	cmd, args := parseCommand("   ")
	if cmd != "" || args != nil {
		t.Fatalf("expected empty, got %q %#v", cmd, args)
	}
}

func TestParseCommandStatsName(t *testing.T) {
	cmd, args := parseCommand("/stats Survival")
	if cmd != "/stats" {
		t.Fatalf("cmd = %q", cmd)
	}
	if !reflect.DeepEqual(args, []string{"Survival"}) {
		t.Fatalf("args = %#v", args)
	}
}
