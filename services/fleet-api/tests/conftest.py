import os
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
REPO = ROOT.parents[1]
os.environ.setdefault("CURSOR_SECRET", "test-cursor-secret")
