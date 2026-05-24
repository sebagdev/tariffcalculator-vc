# Tariff Comparator

Go + HTMX web app for projecting hourly electricity usage and comparing Polish TAURON G11, G12, G12w, and G13 tariffs. You can paste one or more hourly CSV exports into the browser and get a ranked annual cost summary.

## Run The Web App

```sh
go run . --data ../docs --year 2026 --phase 3 --power-kw 1
```

Then open:

```text
http://127.0.0.1:8080
```

The textarea is prefilled from `../docs` when those CSVs are available. You can replace it with copied CSV exports and press `Compare tariffs`.

Useful web flags:

```text
--addr                 Web address. Default: 127.0.0.1:8080
--data                 Directory with CSV exports. Default: ../docs
--year                 Calendar year to project. Default: current year
--phase                1 or 3, used for fixed distribution fees. Default: 3
--power-kw             Contracted power in kW. Default: 1
--billing-months       Seller billing cycle: 1, 2, 6, or 12. Default: 12
```

## Run The CLI

```sh
go run . --cli --data ../docs --year 2026 --phase 3 --power-kw 1
```

CLI-only flag:

```text
--show-hourly-profile  Print observed weekday/hour averages
```

## Build A Binary

```sh
make build
./dist/tariff-comparator --addr 127.0.0.1:8080
```

Equivalent without `make`:

```sh
mkdir -p dist
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/tariff-comparator .
```

## Build A Docker Image

```sh
docker build -t tariffcalculator-vc:local .
docker run --rm -p 8080:8080 -v "$(pwd)/../docs:/app/docs:ro" tariffcalculator-vc:local
```

Or with Compose:

```sh
docker compose up --build
```

The container listens on port `8080` and exposes a health endpoint:

```text
http://127.0.0.1:8080/healthz
```

## GitHub Actions

This repo includes two workflows:

- `.github/workflows/ci.yml` runs format checks, Go tests, a binary build, and a Docker build on pushes and pull requests.
- `.github/workflows/docker-publish.yml` builds and publishes the Docker image to GitHub Container Registry as `ghcr.io/<owner>/<repo>` on pushes to `main`/`master`, version tags, or manual runs.

For your repo, the published image will be:

```text
ghcr.io/sebagdev/tariffcalculator-vc
```

After pushing to GitHub, check the repository `Actions` tab. If the package is private and you want to pull it elsewhere, configure package visibility or authenticate with GitHub Container Registry.

## Push Into Your Existing Empty Repo

From this machine, after cloning `https://github.com/sebagdev/tariffcalculator-vc`:

```sh
git clone https://github.com/sebagdev/tariffcalculator-vc.git
cd tariffcalculator-vc
rsync -av --exclude .git --exclude dist --exclude .gitignore /Users/sg/vibecoding/tariff_comparator/ .
printf "\n# Tariff calculator\n" >> .gitignore
cat /Users/sg/vibecoding/tariff_comparator/.gitignore >> .gitignore
git add .
git commit -m "Add tariff calculator web app"
git push origin main
```

If the repository default branch is `master`, push to `master` instead.

## Projection model

The app learns average usage for each `(day of week, hour of day)` from the CSVs, then builds an hourly projection for the requested year. If a weekday/hour combination is missing, it falls back to the average for that hour across all observed days, then to the global average.

Duplicate hourly records are skipped when they have the same timestamp and kWh value. Conflicting duplicates stop the run.

CSV timestamps such as `1:00` are treated as the hour interval starting at `00:00`; `24:00` is treated as `23:00`.

## Tariff assumptions

Rates are encoded from `docs/deep-research-report.md`:

- VAT: 23%
- common variable net fees: quality `0.0331`, OZE `0.00730`, cogeneration `0.00030`, capacity `0.2194` PLN/kWh
- fixed distribution fee and distribution subscription: `7.38` PLN net/month for 1-phase, `10.86` PLN net/month for 3-phase
- seller subscription gross/month by billing cycle: `12 -> 0.47`, `6 -> 0.93`, `2 -> 2.80`, `1 -> 5.61`

Time-zone windows used by the calculator:

- G11: all hours in one zone
- G12 day: weekdays and weekends `06:00-13:00`, `15:00-22:00`; night is the remaining time
- G12w peak: weekdays `06:00-13:00`, `15:00-22:00`; off-peak is nights plus all weekend hours
- G13 morning peak: weekdays `07:00-13:00`
- G13 afternoon peak: weekdays `16:00-21:00` in October-March, `19:00-22:00` in April-September
- G13 other: all remaining hours, including weekends

The markdown report did not include explicit G13 time windows, so those are kept in `main.go` as editable assumptions.
