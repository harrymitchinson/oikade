package home

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	homev1alpha1 "github.com/harrymitchinson/oikade/api/home/v1alpha1"
	"github.com/harrymitchinson/oikade/internal/controller/shared"
)

// maxNameLen keeps a generated Device name short enough for a label value and the topics and
// friendly names derived from it.
const maxNameLen = 63

// slug turns a name from an app into a Device or Room name: lower case letters and digits, with
// a single '-' between words. Letters outside ASCII are dropped. It may return "".
func slug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return trimName(b.String(), maxNameLen)
}

// trimName cuts a slug to n bytes without leaving a trailing '-'.
func trimName(s string, n int) string {
	if len(s) > n {
		s = s[:n]
	}
	return strings.TrimRight(s, "-")
}

// lastFour is the last four letters or digits of an ID, lower case.
func lastFour(id string) string {
	s := strings.ReplaceAll(slug(id), "-", "")
	if len(s) > 4 {
		s = s[len(s)-4:]
	}
	return s
}

// adoptedName chooses the name of a Device for a discovered device: the slug of its name in the
// app; on a clash, prefixed with its room's; then with a number. Without a name, it's the model
// and the ID's last four characters. taken holds the Device names already in the namespace.
func adoptedName(d homev1alpha1.DiscoveredDevice, taken map[string]bool) string {
	base := slug(d.Name)
	if base == "" {
		model := slug(d.Model)
		if model == "" {
			model = "device"
		}
		base = trimName(model, maxNameLen-5) + "-" + lastFour(d.ID)
	}
	if !taken[base] {
		return base
	}
	if room := slug(d.Room); room != "" && !strings.HasPrefix(base, room+"-") {
		base = trimName(room+"-"+base, maxNameLen)
		if !taken[base] {
			return base
		}
	}
	for n := 2; ; n++ {
		suffix := "-" + strconv.Itoa(n)
		name := trimName(base, maxNameLen-len(suffix)) + suffix
		if !taken[name] {
			return name
		}
	}
}

// roomFor matches a room named in an app to an existing Room, by its name or displayName. Rooms
// are never created from an app: with no match, the Device has no room.
func roomFor(name string, rooms []homev1alpha1.Room) *corev1.LocalObjectReference {
	if name == "" {
		return nil
	}
	s := slug(name)
	for _, r := range rooms {
		if r.Name == s || (r.Spec.DisplayName != "" && strings.EqualFold(r.Spec.DisplayName, strings.TrimSpace(name))) {
			return &corev1.LocalObjectReference{Name: r.Name}
		}
	}
	return nil
}

// maxDisplayName is the Device CRD's limit on displayName, in characters.
const maxDisplayName = 64

// displayName is a name from an app cut to fit Device.spec.displayName.
func displayName(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxDisplayName {
		return s
	}
	return string([]rune(s)[:maxDisplayName])
}

// takenOver reports whether something other than the operator, such as Flux, Argo CD or kubectl
// apply, has written a Device's spec. The Device is then the user's, not adopted.
func takenOver(d *homev1alpha1.Device) bool {
	for _, f := range d.ManagedFields {
		if f.Manager == shared.FieldOwner || f.Subresource != "" || f.FieldsV1 == nil {
			continue
		}
		if f.Operation == metav1.ManagedFieldsOperationApply || f.Operation == metav1.ManagedFieldsOperationUpdate {
			if strings.Contains(f.FieldsV1.GetRawString(), `"f:spec"`) {
				return true
			}
		}
	}
	return false
}
