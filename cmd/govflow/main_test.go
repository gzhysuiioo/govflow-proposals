package main

import (
	"errors"
	"testing"

	"github.com/gzhysuiioo/govflow-proposals/govflow"
)

func roster(ids ...string) []govflow.VoteMember {
	members := make([]govflow.VoteMember, len(ids))
	for i, id := range ids {
		members[i] = govflow.VoteMember{ID: id, Weight: 100}
	}
	return members
}

func TestResolveDelegationsPlainPairs(t *testing.T) {
	got, err := resolveDelegations([]string{"bob:alice", "carol:alice"}, roster("alice", "bob", "carol"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := []govflow.Delegation{{From: "bob", To: "alice"}, {From: "carol", To: "alice"}}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("delegation %d = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestResolveDelegationColonIDs(t *testing.T) {
	// 名单只有 team:alice 和 bob：team:alice:bob 表示前者委托给后者。
	got, err := resolveDelegations([]string{"team:alice:bob"}, roster("team:alice", "bob"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := govflow.Delegation{From: "team:alice", To: "bob"}
	if got[0] != want {
		t.Fatalf("got %v, want %v", got[0], want)
	}
}

func TestResolveDelegationBothEndsColonIDs(t *testing.T) {
	// 两端都含冒号：team:alice 委托给 team:bob。
	got, err := resolveDelegations([]string{"team:alice:team:bob"}, roster("team:alice", "team:bob"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	want := govflow.Delegation{From: "team:alice", To: "team:bob"}
	if got[0] != want {
		t.Fatalf("got %v, want %v", got[0], want)
	}
}

func TestResolveDelegationAmbiguous(t *testing.T) {
	// a:b:c 既可表示 a -> b:c，也可表示 a:b -> c，必须拒绝且属于参数错误。
	_, err := resolveDelegations([]string{"a:b:c"}, roster("a", "a:b", "b:c", "c"))
	if err == nil {
		t.Fatal("expected ambiguity error")
	}
	var usageErr *delegationUsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("error %v is not a usage error", err)
	}
}

func TestResolveDelegationNoMatch(t *testing.T) {
	// 格式正确但两端无法同时命中名单：域错误，不是参数错误。
	_, err := resolveDelegations([]string{"alice:ghost"}, roster("alice", "bob"))
	if err == nil {
		t.Fatal("expected no-match error")
	}
	if !errors.Is(err, govflow.ErrInvalidProposal) {
		t.Fatalf("error %v is not ErrInvalidProposal", err)
	}
	var usageErr *delegationUsageError
	if errors.As(err, &usageErr) {
		t.Fatalf("error %v must not be a usage error", err)
	}
}

func TestResolveDelegationMalformed(t *testing.T) {
	for _, arg := range []string{"alicebob", ":alice", "alice:"} {
		_, err := resolveDelegations([]string{arg}, roster("alice", "bob"))
		if err == nil {
			t.Fatalf("%q: expected malformed error", arg)
		}
		var usageErr *delegationUsageError
		if !errors.As(err, &usageErr) {
			t.Fatalf("%q: error %v is not a usage error", arg, err)
		}
	}
}

func TestResolveDelegationCaseSensitive(t *testing.T) {
	// 编号区分大小写：Team 与 team 是不同成员。
	_, err := resolveDelegations([]string{"team:bob"}, roster("Team", "bob"))
	if !errors.Is(err, govflow.ErrInvalidProposal) {
		t.Fatalf("expected no-match domain error, got %v", err)
	}
}

func TestResolveDelegationSelfPairLeftToBusinessRules(t *testing.T) {
	// 自委托在解析层能唯一对应名单成员，拒绝它是业务规则的职责。
	got, err := resolveDelegations([]string{"a:a"}, roster("a", "b"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got[0] != (govflow.Delegation{From: "a", To: "a"}) {
		t.Fatalf("got %v", got[0])
	}
}
