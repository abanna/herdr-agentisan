package settings

import "slices"

// RepoFileKeys are the only keys a repo file may set.
var RepoFileKeys = slices.Clone(repoFileKeys)

// ProfileKeys are the leaf key paths of a profile body, the schema a repo
// file is checked against, each with the kind of its value: string, int,
// strings, ints, argvs or secret. A * stands for any one name.
func ProfileKeys() map[string]string {
	out := map[string]string{}
	var walk func(n *node, path string)
	walk = func(n *node, path string) {
		switch n.kind {
		case kindTable:
			for k, f := range n.fields {
				walk(f, join(path, k))
			}
		case kindNames:
			walk(n.elem, join(path, "*"))
		default:
			out[path] = map[kind]string{
				kindString: "string", kindInt: "int", kindStrings: "strings",
				kindInts: "ints", kindArgvs: "argvs", kindSecret: "secret",
			}[n.kind]
		}
	}
	walk(profileSchema, "")
	return out
}
