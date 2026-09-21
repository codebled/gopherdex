// Package hello returns friendly greetings. It is the sample module served by
// Gopherdex so you can test GOPROXY end to end.
package hello

import "fmt"

// Greeting returns a greeting for name.
func Greeting(name string) string {
	return fmt.Sprintf("Hello, %s!", name)
}
