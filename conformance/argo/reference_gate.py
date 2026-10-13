"""Wait for a real S3 gate using only dependencies shipped in the runner image."""

from __future__ import annotations

import argparse
import time

import botocore.session
from botocore.exceptions import ClientError


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("bucket")
    parser.add_argument("key")
    parser.add_argument("endpoint")
    args = parser.parse_args()
    client = botocore.session.get_session().create_client(
        "s3", endpoint_url=args.endpoint, region_name="us-east-1"
    )
    waiting = False
    while True:
        try:
            client.head_object(Bucket=args.bucket, Key=args.key)
        except ClientError as error:
            if error.response["ResponseMetadata"]["HTTPStatusCode"] != 404:
                raise
            if not waiting:
                print("REFERENCE_GATE_WAITING", flush=True)
                waiting = True
            time.sleep(0.2)
        else:
            print("REFERENCE_GATE_RELEASED", flush=True)
            return


if __name__ == "__main__":
    main()
