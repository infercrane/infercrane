from __future__ import annotations

import pytest

from infercrane.application import ControlPlane
from infercrane.persistence import Database
from infercrane.settings import Settings


@pytest.fixture
def settings(tmp_path):
    return Settings(state_dir=tmp_path, database_url=f"sqlite:///{tmp_path / 'state.db'}")


@pytest.fixture
def database(settings):
    db = Database(settings)
    db.create_schema()
    return db


@pytest.fixture
def control(database):
    return ControlPlane(database)
