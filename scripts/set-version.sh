#!/usr/bin/env bash
# Stamps a release version into every package manifest.
#   scripts/set-version.sh 0.2.0
set -euo pipefail
cd "$(dirname "$0")/.."
v=$1
python3 - "$v" <<'PY'
import json, re, sys, pathlib
v = sys.argv[1]
for p in pathlib.Path("packages/node").rglob("package.json"):
    if "node_modules" in p.parts:
        continue
    d = json.loads(p.read_text())
    d["version"] = v
    for dep in d.get("optionalDependencies", {}):
        d["optionalDependencies"][dep] = v
    p.write_text(json.dumps(d, indent=2) + "\n")
py = pathlib.Path("packages/python/pyproject.toml")
py.write_text(re.sub(r'^version = ".*"$', f'version = "{v}"', py.read_text(), flags=re.M))
init = pathlib.Path("packages/python/os_mem/__init__.py")
init.write_text(re.sub(r'^__version__ = ".*"$', f'__version__ = "{v}"', init.read_text(), flags=re.M))
for pom in [pathlib.Path("packages/java/pom.xml"), pathlib.Path("packages/java/osmem/pom.xml"), pathlib.Path("packages/java/binaries/pom.xml")]:
    # the parent's own <version> and the <parent><version> of the modules
    pom.write_text(re.sub(r"(<artifactId>osmem-parent</artifactId>\s*<version>)[^<]+(</version>)", rf"\g<1>{v}\g<2>", pom.read_text(), count=1))
print("version set to", v)
PY
