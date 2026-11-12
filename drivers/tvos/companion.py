"""Send an actual Siri Remote touch gesture to an explicitly paired physical TV.

Credentials are read from a private file, never passed in process arguments or logged.
Requires pyatv; see docs/tvos.md for setup.
"""
import asyncio
import ipaddress
import json
import logging
import sys

async def swipe(touch, x1, y1, x2, y2, duration):
    # pyatv 0.18's swipe sends the first Hold immediately after Press and can
    # overshoot the endpoint (upstream issue #2914). Use the public action API
    # with spaced events and bounded, monotonic interpolation instead.
    from pyatv.const import TouchAction
    loop = asyncio.get_running_loop()
    seconds = duration / 1000
    await touch.action(x1, y1, TouchAction.Press)
    started = loop.time()
    try:
        while True:
            await asyncio.sleep(min(0.016, max(0, seconds - (loop.time() - started))))
            fraction = min(1, (loop.time() - started) / seconds)
            if fraction >= 1:
                break
            await touch.action(round(x1 + (x2-x1)*fraction),
                               round(y1 + (y2-y1)*fraction), TouchAction.Hold)
    finally:
        await touch.action(x2, y2, TouchAction.Release)

async def command(tv, raw):
    button = raw[1] if len(raw) == 2 and raw[0] == "press" else None
    coords = [] if button else [int(value) for value in raw]
    if button:
        if button not in {"up", "down", "left", "right", "select", "menu", "home", "play_pause"}:
            raise ValueError("unsupported remote button")
        await getattr(tv.remote_control, button)()
    else:
        if len(coords) != 5 or any(v < 0 or v > 1000 for v in coords[:4]) or not 1 <= coords[4] <= 10000:
            raise ValueError("invalid touch coordinates or duration")
        x1, y1, x2, y2, duration = coords
        # A Companion touch begins at the virtual pad's center. Preserve the
        # requested displacement, scaled to keep both endpoints on the pad.
        await swipe(tv.touch, 500, 500, 500+(x2-x1)//2, 500+(y2-y1)//2, duration)
        await asyncio.sleep(0.1)

async def main():
    import pyatv
    from pyatv.conf import AppleTV, ManualService
    from pyatv.const import Protocol

    config_path, udid, *raw = sys.argv[1:]
    with open(config_path, encoding="utf-8") as stream:
        target = json.load(stream)[udid]
    config = AppleTV(ipaddress.ip_address(target["address"]), "Maestro TV")
    config.add_service(ManualService(target["identifier"], Protocol.Companion,
                                    int(target.get("port", 49152)), {},
                                    credentials=target["credentials"]))
    tv = await asyncio.wait_for(pyatv.connect(config, asyncio.get_running_loop()), 20)
    try:
        # Complete lazy Companion setup before generating touch timestamps.
        await asyncio.wait_for(tv.apps.app_list(), 20)
        # tvOS acknowledges setup before its new touch session accepts input.
        # Settle once per connection; subsequent gestures reuse the ready session.
        await asyncio.sleep(0.5)
        if raw != ["serve"]:
            await asyncio.wait_for(command(tv, raw), 20)
            return
        while True:
            line = await asyncio.to_thread(sys.stdin.readline)
            if not line:
                break
            try:
                await asyncio.wait_for(command(tv, json.loads(line)), 20)
                result = {"ok": True}
            except Exception as error:
                result = {"ok": False, "error": type(error).__name__}
            print(json.dumps(result), flush=True)
    finally:
        tasks = tv.close()
        if tasks:
            await asyncio.gather(*tasks, return_exceptions=True)

if __name__ == "__main__":
    # Protocol logs may contain pairing material; errors identify only the class.
    logging.disable(logging.CRITICAL)
    try:
        asyncio.run(main())
    except Exception as error:
        print("Companion swipe failed (" + type(error).__name__ + "); check the private TV configuration and pairing.", file=sys.stderr)
        sys.exit(1)
