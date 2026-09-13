"""pytest fixtures for osmem.

Configure the seed in pytest.ini / pyproject.toml::

    [tool.pytest.ini_options]
    osmem_seed = ["testdata/seed"]
    osmem_freeze = true

or override the ``osmem_server`` fixture in conftest.py to call
``OsmemServer.start`` yourself. Tests take ``osmem_clone`` (a fresh clone
per test, closed afterwards) or ``osmem_server`` (shared, read-only use).
"""

from __future__ import annotations

import pytest

from os_mem import OsmemServer


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addini("osmem_seed", "osmem seed directories or .ndjson files", type="paths", default=[])
    parser.addini("osmem_freeze", "freeze the osmem base after seeding", type="bool", default=True)
    parser.addini("osmem_japanese", "enable Japanese analysis in osmem", type="bool", default=True)


@pytest.fixture(scope="session")
def osmem_server(request: pytest.FixtureRequest):
    """A session-wide osmem-server seeded from the osmem_seed ini option."""
    cfg = request.config
    server = OsmemServer.start(
        seed=[str(p) for p in cfg.getini("osmem_seed")],
        freeze=cfg.getini("osmem_freeze"),
        japanese=cfg.getini("osmem_japanese"),
    )
    yield server
    server.close()


@pytest.fixture
def osmem_clone(osmem_server: OsmemServer):
    """A clone of the base for one test; use ``osmem_clone.url`` with a client."""
    clone = osmem_server.clone()
    yield clone
    clone.close()


@pytest.fixture
def osmem_url(osmem_clone) -> str:
    """The clone URL, for tests that only need the address."""
    return osmem_clone.url
