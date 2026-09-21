// Package hello returns friendly greetings. It is the sample module served by
// Gopherdex so you can test GOPROXY end to end.
package hello

import "fmt"

// Greeting returns a greeting for name. An empty name greets the world.
func Greeting(name string) string {
	if name == "" {
		name = "world"
	}
	return fmt.Sprintf("Hello, %s!", name)
}

// Greeter builds greetings in a fixed style.
type Greeter struct {
	// Exclaim ends greetings with "!" instead of ".".
	Exclaim bool
}

// NewGreeter returns a Greeter that exclaims.
func NewGreeter() *Greeter {
	return &Greeter{Exclaim: true}
}

// Greet returns a greeting for name in g's style.
func (g *Greeter) Greet(name string) string {
	s := Greeting(name)
	if !g.Exclaim {
		s = s[:len(s)-1] + "."
	}
	return s
}
