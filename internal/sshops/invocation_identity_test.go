package sshops

import "testing"

func TestInvocationIdentityDoesNotDependOnPlannerRewording(t *testing.T) {
	first := hashInvocationTask("call-1", "检查服务")
	if first != hashInvocationTask("call-1", "重装依赖") {
		t.Fatal("the same canonical invocation must collide even when its arguments change")
	}
	if first == hashInvocationTask("call-2", "检查服务") {
		t.Fatal("a new invocation after another tool must not collide with the first")
	}
	if hashInvocationTask("", "检查服务") != hashTask("检查服务") {
		t.Fatal("legacy callers retain their existing task key")
	}
}
