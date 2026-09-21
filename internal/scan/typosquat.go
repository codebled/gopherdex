package scan

import (
	"fmt"
	"strings"
)

// WellKnown are widely used public Go modules, as owner/repo. A module
// here with the same owner/repo, or one typo away, is probably trying to
// be mistaken for them.
var WellKnown = []string{
	"gin-gonic/gin", "gorilla/mux", "gorilla/websocket", "labstack/echo", "gofiber/fiber", "go-chi/chi",
	"spf13/cobra", "spf13/viper", "spf13/pflag", "urfave/cli", "alecthomas/kingpin",
	"stretchr/testify", "onsi/ginkgo", "onsi/gomega", "golang/mock", "google/go-cmp",
	"sirupsen/logrus", "uber-go/zap", "rs/zerolog", "go-kit/kit",
	"google/uuid", "gofrs/uuid", "satori/go.uuid", "pkg/errors", "hashicorp/go-multierror",
	"go-sql-driver/mysql", "lib/pq", "jackc/pgx", "mattn/go-sqlite3", "go-gorm/gorm", "jmoiron/sqlx", "go-redis/redis", "redis/go-redis",
	"golang-jwt/jwt", "dgrijalva/jwt-go", "golang/protobuf", "grpc/grpc-go", "gogo/protobuf",
	"prometheus/client_golang", "open-telemetry/opentelemetry-go", "kubernetes/client-go",
	"aws/aws-sdk-go", "aws/aws-sdk-go-v2", "Azure/azure-sdk-for-go", "googleapis/google-cloud-go",
	"docker/docker", "hashicorp/terraform", "hashicorp/consul", "hashicorp/vault", "etcd-io/etcd",
	"BurntSushi/toml", "go-yaml/yaml", "json-iterator/go", "tidwall/gjson", "mitchellh/mapstructure",
	"joho/godotenv", "kelseyhightower/envconfig", "fatih/color", "charmbracelet/bubbletea", "charmbracelet/lipgloss",
	"golang/go", "golang/tools", "golang/crypto", "golang/net", "golang/sys", "golang/text", "golang/sync",
}

// Typosquat checks a new module's owner/name against popular modules.
// popular are owner/name pairs of modules on the registry itself;
// WellKnown is checked too.
func Typosquat(ownerName string, popular []string) []Finding {
	key := strings.ToLower(ownerName)
	var out []Finding
	seen := map[string]bool{}
	check := func(target string, hosted bool) {
		t := strings.ToLower(target)
		if seen[t] {
			return
		}
		seen[t] = true
		switch {
		case t == key && !hosted:
			out = append(out, Finding{Rule: "impersonation", Severity: Warn,
				Message: fmt.Sprintf("%s has the same owner and name as the well-known module github.com/%s.", ownerName, target)})
		case t != key && nearlyEqual(key, t):
			where := "the popular module " + target + " on this registry"
			if !hosted {
				where = "the well-known module github.com/" + target
			}
			out = append(out, Finding{Rule: "typosquatting", Severity: Warn,
				Message: fmt.Sprintf("%s is one typo away from %s.", ownerName, where)})
		}
	}
	for _, p := range popular {
		check(p, true)
	}
	for _, w := range WellKnown {
		check(w, false)
	}
	return out
}

// nearlyEqual reports whether a and b differ by a single edit (insert, delete,
// substitute or swap two neighbors), or two edits for long names. Short
// names are left alone: they collide by accident too often.
func nearlyEqual(a, b string) bool {
	if len(a) < 6 || len(b) < 6 {
		return false
	}
	limit := 1
	if len(a) >= 14 {
		limit = 2
	}
	d := len(a) - len(b)
	if d > limit || -d > limit {
		return false
	}
	return distance(a, b) <= limit
}

// distance is the optimal string alignment distance (Damerau-Levenshtein
// without repeated edits of one substring).
func distance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev2 := make([]int, len(rb)+1)
	prev := make([]int, len(rb)+1)
	cur := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
			if i > 1 && j > 1 && ra[i-1] == rb[j-2] && ra[i-2] == rb[j-1] {
				cur[j] = min(cur[j], prev2[j-2]+1)
			}
		}
		prev2, prev, cur = prev, cur, prev2
	}
	return prev[len(rb)]
}
