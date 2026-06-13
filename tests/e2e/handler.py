import json
import sys
from datetime import datetime
def main():
    result = {
        "message": "Hello from PandaStack Functions!",
        "timestamp": datetime.utcnow().isoformat() + "Z",
        "python": sys.version.split()[0],
    }
    print(json.dumps(result, indent=2))
if __name__ == "__main__":
    main()
