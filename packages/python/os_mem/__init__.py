"""In-memory OpenSearch-compatible server for tests.

    from os_mem import OsmemServer

    with OsmemServer.start(seed=["testdata/seed"], freeze=True) as server:
        with server.clone() as clone:
            client = OpenSearch(hosts=[clone.url])
            ...

The pytest plugin (loaded automatically) provides the ``osmem_server``
(session) and ``osmem_clone`` (function) fixtures; see ``os_mem.pytest_plugin``.
"""

from __future__ import annotations

import json
import os
import platform
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Iterable, Optional, Sequence, Union

__all__ = ["OsmemServer", "OsmemClone", "OsmemError", "resolve_binary"]

__version__ = "0.1.0"


class OsmemError(RuntimeError):
    """Raised when the server cannot be started or a request fails."""


def resolve_binary(binary: Optional[str] = None) -> str:
    """Locate osmem-server: explicit path, OSMEM_SERVER_BIN, then the bundled binary."""
    if binary:
        return binary
    env = os.environ.get("OSMEM_SERVER_BIN")
    if env:
        return env
    name = "osmem-server.exe" if sys.platform == "win32" else "osmem-server"
    bundled = Path(__file__).parent / "bin" / name
    if bundled.exists():
        return str(bundled)
    raise OsmemError(
        f"osmem: no server binary bundled for {sys.platform}/{platform.machine()}; "
        "install a platform wheel or set OSMEM_SERVER_BIN"
    )


def _request(method: str, url: str, body: Any = None, timeout: float = 30.0) -> Any:
    data = None if body is None else json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, method=method, headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            text = resp.read().decode()
    except urllib.error.HTTPError as e:
        text = e.read().decode()
        try:
            err = json.loads(text).get("error", text)
            detail = f"{err.get('type')}: {err.get('reason')}" if isinstance(err, dict) else str(err)
        except ValueError:
            detail = text
        raise OsmemError(f"osmem: {method} {url}: {e.code} {detail}") from None
    return json.loads(text) if text else {}


@dataclass
class OsmemClone:
    """A clone of the base cluster served on its own port."""

    server: "OsmemServer"
    id: str
    url: str

    def close(self) -> None:
        try:
            _request("DELETE", f"{self.server.url}/_osmem/clones/{self.id}")
        except OsmemError:
            pass

    def __enter__(self) -> "OsmemClone":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()


@dataclass
class OsmemServer:
    """A running osmem-server process hosting a base cluster."""

    process: subprocess.Popen
    url: str
    pid: int
    version: str
    japanese: bool
    indices: list = field(default_factory=list)

    @classmethod
    def start(
        cls,
        seed: Union[None, str, os.PathLike, Sequence[Union[str, os.PathLike]]] = None,
        *,
        freeze: bool = False,
        japanese: bool = True,
        addr: Optional[str] = None,
        binary: Optional[str] = None,
        startup_timeout: float = 30.0,
    ) -> "OsmemServer":
        """Start the server and wait until it is ready."""
        args = [resolve_binary(binary), "--parent-pid", str(os.getpid())]
        seeds: Iterable[Any] = [] if seed is None else ([seed] if isinstance(seed, (str, os.PathLike)) else seed)
        for s in seeds:
            args += ["--seed", os.fspath(s)]
        if freeze:
            args.append("--freeze")
        if not japanese:
            args.append("--no-ja")
        if addr:
            args += ["--addr", addr]
        proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=None, text=True)
        ready: dict = {}
        error: list = []

        def reader() -> None:
            assert proc.stdout is not None
            for line in proc.stdout:
                try:
                    msg = json.loads(line)
                except ValueError:
                    continue
                if isinstance(msg, dict) and "url" in msg:
                    ready.update(msg)
                    return
            error.append("server exited during startup")

        t = threading.Thread(target=reader, daemon=True)
        t.start()
        t.join(startup_timeout)
        if not ready:
            proc.kill()
            raise OsmemError("osmem: " + (error[0] if error else f"server did not start within {startup_timeout} s"))
        return cls(proc, ready["url"], ready.get("pid", proc.pid), ready.get("version", ""), ready.get("japanese", False), list(ready.get("indices", [])))

    def clone(self) -> OsmemClone:
        """Create a clone served on its own port; freezes the base."""
        res = _request("POST", f"{self.url}/_osmem/clones")
        return OsmemClone(self, res["id"], res["url"])

    def freeze(self) -> None:
        _request("POST", f"{self.url}/_osmem/base/freeze")

    def request(self, method: str, path: str, body: Any = None) -> Any:
        """Any request against the base, decoded as JSON; raises OsmemError on error status."""
        return _request(method, f"{self.url}{path}", body)

    def close(self, grace: float = 5.0) -> None:
        """Stop the process: close stdin, then kill after the grace period."""
        if self.process.poll() is not None:
            return
        try:
            if self.process.stdin:
                self.process.stdin.close()
        except OSError:
            pass
        deadline = time.monotonic() + grace
        while self.process.poll() is None and time.monotonic() < deadline:
            time.sleep(0.05)
        if self.process.poll() is None:
            self.process.kill()
            self.process.wait()

    def __enter__(self) -> "OsmemServer":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()
