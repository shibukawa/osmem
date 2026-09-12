import os
from pathlib import Path

if not os.environ.get("OSMEM_SERVER_BIN"):
    for p in (Path(__file__).resolve().parents[3] / "dist").glob("*/osmem-server*"):
        os.environ["OSMEM_SERVER_BIN"] = str(p)
        break
