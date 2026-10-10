package execenv

import (
	"net/url"
	"strings"
)

func isGongfengRepository(raw string) bool {
	if u, err := url.Parse(raw); err == nil && strings.EqualFold(u.Hostname(), "git.code.tencent.com") {
		return true
	}
	// Git's SCP-style SSH remotes have no URL scheme.
	host, _, ok := strings.Cut(raw, ":")
	return ok && strings.EqualFold(host, "git@git.code.tencent.com")
}

func writeGongfengGuidance(b *strings.Builder) {
	b.WriteString("\nTencent Gongfeng (`git.code.tencent.com`): use `git` to push and `gongfeng` to open/read merge requests. The runtime host needs its own Git credentials and Gongfeng Private Token; Multica's connection token is not passed to agents. ")
	b.WriteString("Read `gongfeng --help` and `gongfeng mr create --help` first. In the repository checkout, use `gongfeng --project-id <namespace/project> mr create --source-branch <current-branch> --target-branch <target-branch> --title \"<issue-identifier> ...\"`; put `Closes <issue-identifier>` in the MR description when it should complete the issue. ")
	b.WriteString("Use `gongfeng --project-id <namespace/project> mr list --json` to find an MR before updating it. The CLI's show/update/accept commands take the global MR `id`, while Multica displays the repository-local `iid`; never substitute one for the other. If the CLI is unavailable or credentials are missing, report the prerequisite instead of claiming an MR was created. See the platform skill's `references/gongfeng.md`.\n")
}
