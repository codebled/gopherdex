// Package license recognizes open source licenses in license files.
package license

import (
	"sort"

	"github.com/google/licensecheck"
)

// Detect returns the SPDX IDs of the licenses in a license file, using the
// same scanner as pkg.go.dev. A file must be mostly (75%) license text for
// the result to count.
func Detect(text []byte) []string {
	cov := licensecheck.Scan(text)
	if cov.Percent < 75 {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, m := range cov.Match {
		if !seen[m.ID] {
			seen[m.ID] = true
			ids = append(ids, m.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// FileNames are the root files checked for a license, in order.
var FileNames = []string{"LICENSE", "LICENSE.md", "LICENSE.txt", "LICENCE", "LICENCE.md", "LICENCE.txt", "COPYING", "COPYING.md", "COPYING.txt"} //nolint:misspell // LICENCE is the British spelling some modules use for the file name
