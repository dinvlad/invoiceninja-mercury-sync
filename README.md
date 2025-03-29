# invoiceninja-mercury
Docker image to sync Mercury bank transactions to Invoice Ninja

## Configuration

The configuration is passed as a JSON file with the following structure:

```json
{
  "mercuryAPIKey": "<mercury-api-key>",
  "invoiceNinjaToken": "<invoice-ninja-token>",
  "invoiceNinjaURL": "<invoice-ninja-url>"
}
```

Mercury API key only needs **Read** access to your Mercury account.

## Running

The image can be run with the following command:

```sh
docker run -d -v /path/to/config.json:/config.json:ro \
    ghcr.io/dinvlad/invoiceninja-mercury-sync:main
```
