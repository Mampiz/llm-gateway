package provider

import (
	"context"
	"strings"
	"testing"
)

type fake struct{ name string }

func (f fake) Name() string { return f.name }
func (f fake) Chat(context.Context, ChatRequest) (*ChatResponse, error) {
	return &ChatResponse{Model: f.name}, nil
}

func (f fake) ChatStream(context.Context, ChatRequest) (Stream, error) {
	return nil, ErrStreamingNotSupported
}

func TestRegistry_RoutesByPrefix(t *testing.T) {
	oa := fake{"openai"}
	an := fake{"anthropic"}

	reg := NewRegistry()
	reg.Register(oa, "gpt-", "o1-")
	reg.Register(an, "claude-")

	tests := []struct {
		model string
		want  string
	}{
		{"gpt-4o-mini", "openai"},
		{"o1-preview", "openai"},
		{"claude-sonnet-5", "anthropic"},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			p, err := reg.For(tt.model)
			if err != nil {
				t.Fatalf("For(%q) failed: %v", tt.model, err)
			}
			if p.Name() != tt.want {
				t.Errorf("For(%q) = %q, want %q", tt.model, p.Name(), tt.want)
			}
		})
	}
}

// TestRegistry_LongestPrefixWins guards the rule that a specific route beats a
// general one whatever the registration order, so wiring stays order-free.
func TestRegistry_LongestPrefixWins(t *testing.T) {
	general := fake{"general"}
	specific := fake{"specific"}

	reg := NewRegistry()
	reg.Register(general, "claude-")
	reg.Register(specific, "claude-opus-")

	p, err := reg.For("claude-opus-5")
	if err != nil {
		t.Fatalf("For() failed: %v", err)
	}
	if p.Name() != "specific" {
		t.Errorf("provider = %q, want specific", p.Name())
	}

	// And the reverse registration order must give the same answer.
	reg2 := NewRegistry()
	reg2.Register(specific, "claude-opus-")
	reg2.Register(general, "claude-")
	if p, _ := reg2.For("claude-opus-5"); p.Name() != "specific" {
		t.Errorf("registration order changed the routing: got %q", p.Name())
	}
}

func TestRegistry_UnknownModelWithoutDefault(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fake{"openai"}, "gpt-")

	p, err := reg.For("llama-3")
	if err == nil {
		t.Fatalf("For() = %v, nil; want an error when nothing matches and no default is set", p)
	}
	if !strings.Contains(err.Error(), "llama-3") {
		t.Errorf("error = %q, want it to name the model", err)
	}
}

func TestRegistry_Default(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fake{"openai"}, "gpt-")
	reg.Register(fake{"mock"}, "mock-")

	if err := reg.SetDefault("mock"); err != nil {
		t.Fatalf("SetDefault() failed: %v", err)
	}

	p, err := reg.For("something-unheard-of")
	if err != nil {
		t.Fatalf("For() failed with a default set: %v", err)
	}
	if p.Name() != "mock" {
		t.Errorf("provider = %q, want the default mock", p.Name())
	}
}

func TestRegistry_SetDefaultRejectsUnregistered(t *testing.T) {
	reg := NewRegistry()
	if err := reg.SetDefault("nope"); err == nil {
		t.Error("SetDefault() accepted a provider that was never registered")
	}
}

func TestRegistry_ByNameAndNames(t *testing.T) {
	reg := NewRegistry()
	reg.Register(fake{"openai"}, "gpt-")
	reg.Register(fake{"anthropic"}, "claude-")

	if _, ok := reg.ByName("anthropic"); !ok {
		t.Error("ByName(anthropic) not found")
	}
	if _, ok := reg.ByName("cohere"); ok {
		t.Error("ByName(cohere) found something that was never registered")
	}

	names := reg.Names()
	if len(names) != 2 || names[0] != "anthropic" || names[1] != "openai" {
		t.Errorf("Names() = %v, want [anthropic openai] sorted", names)
	}
}
