from __future__ import annotations

from .config import Settings
from .http import create_server


def main() -> None:
    settings = Settings.from_env()
    server = create_server(settings)
    print(f"obsai listening on http://{settings.bind_host}:{server.server_port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
