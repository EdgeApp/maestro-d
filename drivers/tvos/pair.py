"""Pair a physical TV and save its Companion credentials in a private configuration.

Usage: python pair.py CONFIG_PATH DEVICE_UDID TV_ADDRESS
The device must already be paired with Xcode for WDA automation.
"""
import asyncio
import getpass
import json
import logging
import os
from pathlib import Path
import sys
import tempfile

async def main():
    import pyatv
    from pyatv.const import Protocol
    path, udid, address = sys.argv[1:]
    configs = await pyatv.scan(asyncio.get_running_loop(), hosts=[address])
    if len(configs) != 1:
        raise ValueError("expected one Apple TV at the supplied address")
    config = configs[0]
    pairing = await pyatv.pair(config, Protocol.Companion, asyncio.get_running_loop(), name="Maestro D")
    try:
        await pairing.begin()
        pin = await asyncio.to_thread(getpass.getpass, "Code displayed on the TV: ")
        pairing.pin(pin)
        await pairing.finish()
        if not pairing.has_paired:
            raise RuntimeError("pairing was not confirmed")
        target = Path(path).expanduser()
        data = json.loads(target.read_text()) if target.exists() else {}
        service = pairing.service
        data[udid] = dict(address=str(config.address), identifier=service.identifier,
                          port=service.port, credentials=service.credentials)
        # The file contains credentials. Never print them or create a world-readable file.
        descriptor, temporary = tempfile.mkstemp(prefix=".tv-pair-", dir=target.parent)
        try:
            with os.fdopen(descriptor, "w") as stream:
                json.dump(data, stream, indent=2)
                stream.flush()
                os.fsync(stream.fileno())
            os.replace(temporary, target)
        finally:
            if os.path.exists(temporary):
                os.unlink(temporary)
        print("Paired. Start the Maestro daemon with MAESTRO_TV_REMOTE_CONFIG set to this file.")
    finally:
        await pairing.close()

if __name__ == "__main__":
    logging.disable(logging.CRITICAL)
    try:
        asyncio.run(main())
    except Exception as error:
        print("TV pairing failed (" + type(error).__name__ + "). Check connectivity and the displayed code.", file=sys.stderr)
        sys.exit(1)
