package commands

import (
	"context"
	"fmt"
	"testing"
)

func TestSwitchModel_Success(t *testing.T) {
	rt := &Runtime{
		SwitchModel: func(value string) (string, error) {
			return "old-model", nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch model to gpt-4",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	want := "模型已从 old-model 切换为 gpt-4"
	if reply != want {
		t.Fatalf("reply=%q, want=%q", reply, want)
	}
}

func TestSwitchModel_MissingToKeyword(t *testing.T) {
	rt := &Runtime{
		SwitchModel: func(value string) (string, error) {
			return "old", nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch model gpt-4",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "用法：/switch model to <name>" {
		t.Fatalf("reply=%q, want usage message", reply)
	}
}

func TestSwitchModel_MissingValue(t *testing.T) {
	rt := &Runtime{
		SwitchModel: func(value string) (string, error) {
			return "old", nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch model to",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "用法：/switch model to <name>" {
		t.Fatalf("reply=%q, want usage message", reply)
	}
}

func TestSwitchModel_Error(t *testing.T) {
	rt := &Runtime{
		SwitchModel: func(value string) (string, error) {
			return "", fmt.Errorf("model not found")
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch model to bad-model",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "model not found" {
		t.Fatalf("reply=%q, want error message", reply)
	}
}

func TestSwitchModel_NilDep(t *testing.T) {
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), &Runtime{})

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch model to gpt-4",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "当前上下文不可用该命令。" {
		t.Fatalf("reply=%q, want unavailable message", reply)
	}
}

func TestSwitchChannel_Redirect(t *testing.T) {
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), &Runtime{})

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch channel to telegram",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	want := "该命令已迁移，请使用：/check channel <name>"
	if reply != want {
		t.Fatalf("reply=%q, want=%q", reply, want)
	}
}

func TestCheckChannel_Success(t *testing.T) {
	rt := &Runtime{
		SwitchChannel: func(value string) error {
			return nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/check channel telegram",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	want := "channel「telegram」可用且已启用"
	if reply != want {
		t.Fatalf("reply=%q, want=%q", reply, want)
	}
}

func TestCheckChannel_Error(t *testing.T) {
	rt := &Runtime{
		SwitchChannel: func(value string) error {
			return fmt.Errorf("channel '%s' not found", value)
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/check channel unknown",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "channel 'unknown' not found" {
		t.Fatalf("reply=%q, want error message", reply)
	}
}

func TestCheckChannel_NilDep(t *testing.T) {
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), &Runtime{})

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/check channel telegram",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "当前上下文不可用该命令。" {
		t.Fatalf("reply=%q, want unavailable message", reply)
	}
}

func TestCheckChannel_MissingValue(t *testing.T) {
	rt := &Runtime{
		SwitchChannel: func(value string) error {
			return nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/check channel",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "用法：/check channel <name>" {
		t.Fatalf("reply=%q, want usage message", reply)
	}
}

func TestSwitch_BangPrefix(t *testing.T) {
	rt := &Runtime{
		SwitchModel: func(value string) (string, error) {
			return "old", nil
		},
	}
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), rt)

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "!switch model to gpt-4",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("! prefix: outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	if reply != "模型已从 old 切换为 gpt-4" {
		t.Fatalf("! prefix: reply=%q, want success message", reply)
	}
}

func TestSwitch_NoSubCommand(t *testing.T) {
	ex := NewExecutor(NewRegistry(BuiltinDefinitions()), &Runtime{})

	var reply string
	res := ex.Execute(context.Background(), Request{
		Text: "/switch",
		Reply: func(text string) error {
			reply = text
			return nil
		},
	})
	if res.Outcome != OutcomeHandled {
		t.Fatalf("outcome=%v, want=%v", res.Outcome, OutcomeHandled)
	}
	// Should get usage message from executor's sub-command routing
	if reply == "" {
		t.Fatal("expected usage reply for bare /switch")
	}
}
