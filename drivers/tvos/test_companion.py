"""Transport contract checks without a TV or credentials."""
import asyncio
import importlib.util
import json
from pathlib import Path
import sys
import tempfile
import types
import unittest
from unittest.mock import AsyncMock, Mock, patch

spec = importlib.util.spec_from_file_location('companion', Path(__file__).with_name('companion.py'))
companion = importlib.util.module_from_spec(spec)
spec.loader.exec_module(companion)

class CompanionTests(unittest.TestCase):
    def run_command(self, args, fail=False):
        tv = types.SimpleNamespace(touch=types.SimpleNamespace(action=AsyncMock()),
                                   remote_control=types.SimpleNamespace(select=AsyncMock()),
                                   apps=types.SimpleNamespace(app_list=AsyncMock()),
                                   close=Mock(return_value=set()))
        if fail:
            tv.touch.action.side_effect = RuntimeError('delivery failed')
        connect = AsyncMock(return_value=tv)
        config = Mock()
        modules = {'pyatv': types.SimpleNamespace(connect=connect),
                   'pyatv.conf': types.SimpleNamespace(AppleTV=Mock(return_value=config), ManualService=Mock()),
                   'pyatv.const': types.SimpleNamespace(Protocol=types.SimpleNamespace(Companion='companion'), TouchAction=types.SimpleNamespace(Press=1, Hold=3, Release=4))}
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / 'tv.json'
            path.write_text(json.dumps({'fixture': {'address': '192.0.2.1', 'identifier': 'fixture', 'credentials': 'test'}}))
            with patch.dict(sys.modules, modules), patch.object(sys, 'argv', ['helper', str(path), 'fixture', *args]):
                if fail:
                    with self.assertRaises(RuntimeError): asyncio.run(companion.main())
                else:
                    asyncio.run(companion.main())
        return tv, connect

    def test_coordinates_and_session_closed(self):
        tv, _ = self.run_command(['500', '200', '500', '800', '300'])
        calls = tv.touch.action.await_args_list
        self.assertEqual(calls[0].args, (500, 500, 1))
        self.assertEqual(calls[-1].args, (500, 800, 4))
        self.assertGreater(len(calls), 3)
        ys = [c.args[1] for c in calls]
        self.assertEqual(ys, sorted(ys))
        self.assertTrue(all(500 <= y <= 800 for y in ys))
        tv.close.assert_called_once()

    def test_button(self):
        tv, _ = self.run_command(['press', 'select'])
        tv.remote_control.select.assert_awaited_once()
        tv.touch.action.assert_not_awaited()
        tv.close.assert_called_once()

    def test_delivery_error_closes_session(self):
        tv, _ = self.run_command(['500', '200', '500', '800', '300'], fail=True)
        tv.close.assert_called_once()

    def test_invalid_input(self):
        for args in [['1001', '0', '0', '0', '300'], ['0', '0', '0', '0', '0'], ['press', 'erase']]:
            with self.subTest(args=args), self.assertRaises(ValueError): self.run_command(args)

if __name__ == '__main__': unittest.main()
