#!/usr/bin/env python3
"""
Update Vikunja base version across repository files (Dockerfile and CI workflows).
Usage: ./scripts/update-vikunja-version.py <new_tag> [current_ref]
"""

import os
import re
import sys

def main():
    if len(sys.argv) < 2:
        print("Usage: update-vikunja-version.py <new_tag> [current_ref]", file=sys.stderr)
        sys.exit(1)

    new_tag = sys.argv[1].strip()

    # Validate tag format against semantic version pattern
    tag_pattern = re.compile(r"^v[0-9]+(\.[0-9]+)+(-[0-9A-Za-z.-]+)?$")
    if not tag_pattern.match(new_tag):
        print(f"Error: Provided tag '{new_tag}' does not match expected semver pattern (e.g. v2.7.1)", file=sys.stderr)
        sys.exit(1)

    repo_root = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
    dockerfile_path = os.path.join(repo_root, "Dockerfile")
    ci_workflow_path = os.path.join(repo_root, ".github", "workflows", "ci.yml")

    # Read current ref from Dockerfile if not explicitly passed
    if len(sys.argv) >= 3 and sys.argv[2].strip():
        current_ref = sys.argv[2].strip()
    else:
        with open(dockerfile_path, "r", encoding="utf-8") as f:
            match = re.search(r"^ARG VIKUNJA_REF=(.*)$", f.read(), flags=re.MULTILINE)
            if not match:
                print("Error: Could not determine current VIKUNJA_REF from Dockerfile", file=sys.stderr)
                sys.exit(1)
            current_ref = match.group(1).strip()

    print(f"Updating Vikunja references from '{current_ref}' to '{new_tag}'...")

    # 1. Update Dockerfile
    with open(dockerfile_path, "r", encoding="utf-8") as f:
        dockerfile_content = f.read()

    new_dockerfile_content = re.sub(
        r"^ARG VIKUNJA_REF=.*$",
        f"ARG VIKUNJA_REF={new_tag}",
        dockerfile_content,
        flags=re.MULTILINE,
    )
    with open(dockerfile_path, "w", encoding="utf-8") as f:
        f.write(new_dockerfile_content)

    # 2. Update .github/workflows/ci.yml
    with open(ci_workflow_path, "r", encoding="utf-8") as f:
        ci_content = f.read()

    ci_updated = re.sub(r"ref:\s*" + re.escape(current_ref), f"ref: {new_tag}", ci_content)
    ci_updated = re.sub(r"branch\s+" + re.escape(current_ref), f"branch {new_tag}", ci_updated)
    ci_updated = re.sub(r"Clone Vikunja\s+" + re.escape(current_ref), f"Clone Vikunja {new_tag}", ci_updated)
    ci_updated = re.sub(r"type=raw,value=" + re.escape(current_ref), f"type=raw,value={new_tag}", ci_updated)
    ci_updated = re.sub(r"VIKUNJA_REF=" + re.escape(current_ref), f"VIKUNJA_REF={new_tag}", ci_updated)

    with open(ci_workflow_path, "w", encoding="utf-8") as f:
        f.write(ci_updated)

    print("Successfully updated Dockerfile and CI workflow.")

if __name__ == "__main__":
    main()
