# Still pictures for a Buddy3D camera

A Prusa Buddy3D camera streams H.264 over RTSP (`rtsp://<camera>/live`) and has no HTTP picture. Added as a generic
camera with only a `stream_url`, it shows "This camera has sent no picture yet" on the printer's Cameras tab and on
dashboards until someone presses play, and again after every page change, because the web app shows a camera's latest
still picture by itself and plays the live view only on request.

This runs [go2rtc](https://github.com/AlexxIT/go2rtc) beside the server to make those pictures: it reads the camera
when a picture is asked for and answers a JPEG, on `127.0.0.1` only.

1. Put your camera's address in `go2rtc.yaml` (`streams: buddy3d:`).
2. `docker compose up -d` in this folder, on the machine the server runs on.
3. Check it: `curl -o frame.jpg "http://127.0.0.1:1984/api/frame.jpeg?src=buddy3d"` gives a JPEG (about 3 s, 1280×720).
4. Set the camera up (Printers, the printer, Cameras) with:
   - **Snapshot address**: `http://127.0.0.1:1984/api/frame.jpeg?src=buddy3d`
   - **Stream address**: `rtsp://<camera>/live` (the live view when you press play)

The server must run on the host's network for `127.0.0.1` to be this machine, and for the live view to start at all:
see `compose.host.yml` at the top of the repository. An `rtsp://` snapshot address is refused ("a camera URL is http or
https"), and the camera then doesn't start.
