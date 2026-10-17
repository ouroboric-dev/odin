# Brief: the gallery's version

The gallery's server should tell anyone who asks which version of it is running, and the gallery's web page
should show it.

- The server answers `GET /version` with the JSON `{"version": "0.1.0"}`. Any other method gets a 405.
- The web home page shows the line `API version: 0.1.0` below the line that says whether the API is up.

Nothing else: no login, no galleries, no pages.
