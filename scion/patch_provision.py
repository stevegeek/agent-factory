"""Patch the Claude harness provision.py so agents trust the in-container worktree path.

Scion local mode passes the HOST worktree path as agent_workspace, but the worktree is
mounted at /repo-root/.scion/agents/<name>/workspace inside the container, so Claude
shows its folder-trust prompt. Usage: python3 patch_provision.py <path/to/provision.py>
"""
import sys

path = sys.argv[1]
src = open(path).read()
anchor = "    new_projects: dict[str, Any] = {ctx.workspace: project_settings}\n"
addition = '''
    # agent-factory local patch: in local mode the manifest carries the HOST
    # worktree path, but the worktree is mounted at /repo-root/... inside the
    # container, so trust that path too (upstream fix: pass the container path).
    marker = "/.scion/agents/"
    if marker in ctx.workspace and not ctx.workspace.startswith("/repo-root/"):
        container_ws = "/repo-root" + marker + ctx.workspace.split(marker, 1)[1]
        new_projects[container_ws] = project_settings
'''

if "local patch: in local mode the manifest" in src:
    print("already patched")
    sys.exit(0)
if src.count(anchor) != 1:
    sys.exit("anchor not found; provision.py changed upstream, patch by hand")
open(path, "w").write(src.replace(anchor, anchor + addition))
print("patched")
