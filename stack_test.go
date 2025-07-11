package que

import (
	"strings"
	"testing"
)

type stackS struct{}

func (ss *stackS) stackA() (s string) {
	defer func() {
		e := recover()
		if e != nil {
			s = Stack(3)
		}
	}()
	ss.stackB()
	return
}

func (*stackS) stackB() {
	defer func() {
		e := recover()
		if e != nil {
			panic(e)
		}
	}()
	var i int
	i++
	i *= 5
	panic(i)
}

func TestStack(t *testing.T) {
	ss := &stackS{}
	stack := ss.stackA()
	t.Log(stack)

	stacks := []string{
		"go-que/stack_test.go:36",
		"go-que/stack_test.go:17",
		"go-que/stack_test.go:31",
		"go-que/stack_test.go:25",
		"go-que/stack_test.go:14",
	}
	for _, s := range stacks {
		if !strings.Contains(stack, s) {
			t.Fatalf("want stack has caller: %s", s)
		}
	}
}
