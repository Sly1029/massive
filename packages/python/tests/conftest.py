from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
import uuid
from collections.abc import Generator
from dataclasses import dataclass

import pytest


@dataclass(frozen=True, slots=True)
class S3TestServer:
    endpoint: str
    access_key: str
    secret_key: str


@pytest.fixture(scope="session")
def s3_server() -> Generator[S3TestServer, None, None]:
    configured_endpoint = os.environ.get("MASSIVE_TEST_S3_ENDPOINT")
    if configured_endpoint is not None:
        access_key = os.environ.get("MASSIVE_TEST_S3_ACCESS_KEY")
        secret_key = os.environ.get("MASSIVE_TEST_S3_SECRET_KEY")
        if access_key is None or secret_key is None:
            pytest.skip("configured S3 test endpoint requires access credentials")
        yield S3TestServer(configured_endpoint, access_key, secret_key)
        return

    docker = shutil.which("docker")
    if docker is None:
        pytest.skip("Docker is unavailable; cannot start the real MinIO fixture")

    access_key = "massive-python-test-access"
    secret_key = "massive-python-test-secret"
    container = f"massive-python-minio-{uuid.uuid4().hex}"
    with socket.socket() as listener:
        listener.bind(("127.0.0.1", 0))
        port = listener.getsockname()[1]

    try:
        started = subprocess.run(
            [
                docker,
                "run",
                "-d",
                "--rm",
                "--name",
                container,
                "-p",
                f"127.0.0.1:{port}:9000",
                "-e",
                f"MINIO_ROOT_USER={access_key}",
                "-e",
                f"MINIO_ROOT_PASSWORD={secret_key}",
                "minio/minio",
                "server",
                "/data",
            ],
            check=False,
            capture_output=True,
            text=True,
            timeout=30,
        )
    except (OSError, subprocess.TimeoutExpired) as error:
        pytest.skip(f"could not start the real MinIO fixture: {error}")
    if started.returncode != 0:
        pytest.skip(f"could not start the real MinIO fixture: {started.stderr.strip()}")

    deadline = time.monotonic() + 30
    while time.monotonic() < deadline:
        try:
            with socket.create_connection(("127.0.0.1", port), timeout=0.5):
                break
        except OSError:
            time.sleep(0.1)
    else:
        logs = subprocess.run(
            [docker, "logs", container], check=False, capture_output=True, text=True
        )
        pytest.skip(f"MinIO did not become ready: {logs.stderr}{logs.stdout}")

    try:
        yield S3TestServer(f"http://127.0.0.1:{port}", access_key, secret_key)
    finally:
        subprocess.run(
            [docker, "rm", "-f", container],
            check=False,
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
