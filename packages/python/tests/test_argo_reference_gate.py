from __future__ import annotations

import os
import subprocess
import sys
import uuid
from pathlib import Path

import boto3
from conftest import S3TestServer


def test_live_reference_gate_blocks_on_absence_and_releases_from_real_s3(
    s3_server: S3TestServer,
) -> None:
    client = boto3.client(
        "s3",
        endpoint_url=s3_server.endpoint,
        region_name="us-east-1",
        aws_access_key_id=s3_server.access_key,
        aws_secret_access_key=s3_server.secret_key,
    )
    bucket = f"reference-gate-{uuid.uuid4().hex}"
    client.create_bucket(Bucket=bucket)
    key = "gates/run"
    source = (
        Path(__file__).resolve().parents[3] / "conformance/argo/reference_gate.py"
    ).read_text()
    process = subprocess.Popen(
        [sys.executable, "-c", source, bucket, key, s3_server.endpoint],
        env={
            **os.environ,
            "AWS_ACCESS_KEY_ID": s3_server.access_key,
            "AWS_SECRET_ACCESS_KEY": s3_server.secret_key,
        },
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
    )
    try:
        assert process.stdout is not None
        assert process.stdout.readline().strip() == "REFERENCE_GATE_WAITING"
        assert process.poll() is None
        client.put_object(Bucket=bucket, Key=key, Body=b"go")
        stdout, stderr = process.communicate(timeout=10)
        assert process.returncode == 0, stderr
        assert stdout.strip() == "REFERENCE_GATE_RELEASED"
    finally:
        if process.poll() is None:
            process.kill()
        process.wait(timeout=10)
