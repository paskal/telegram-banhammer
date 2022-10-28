# telegram-banhammer [![Build Status](https://github.com/paskal/telegram-banhammer/workflows/build/badge.svg)](https://github.com/paskal/telegram-banhammer/actions)

A program which uses MTProto Telegram API to ban spammers from a group.

Currently, the only filter is by the join time to kill the bot hoards.

<details><summary>Hoarders example</summary>

![](images/hoard.png)
</details>

## CLI parameters

| Command line        | Default | Description                                                                                |
|---------------------|---------|--------------------------------------------------------------------------------------------|
| appid               |         | AppID, _required_                                                                          |
| apphash             |         | AppHash, _required_                                                                        |
| phone               |         | Telegram phone of the channel admin, _required_                                            |
| password            | ``      | password, if set for the admin, _optional_                                                 |
| channel_id          |         | channel or supergroup id, without -100 part, _required_                                    |
| ban_to_timestamp    |         | the end of the time from which newly joined users will be banned, _required for search_    |
| ban_search_duration |         | amount of time before the ban_to for which we need to ban users, _required for search_     |
| ban_and_kick        |         | set this option to path to text file with users clean up their messages, ban and kick them |
| dbg                 | `false` | debug mode                                                                                 |


## How to run

To get the channel ID, please see https://gist.github.com/mraaroncruz/e76d19f7d61d59419002db54030ebe35, and use it without the `-100` part in the beginning.

To get the AppID and AppHash, please see https://core.telegram.org/api/obtaining_api_id.

After gathering the results, they will be written to a file with the current timestamp in the `ban` directory: no bans will be issued. Feel free to check the results (and remove users you think shouldn't be banned) and rerun the program with `--not-dry-run` flag.

### Gather list of users

`ban_to_timestamp` ([Unix time](https://en.wikipedia.org/wiki/Unix_time) format) and `ban_search_duration` (human-readable duration, like `60s` or `15m`) are mandatory.

#### Docker (recommended)

```bash
docker run --volume=./ban:/srv/ban paskal/telegram-banhammer:master /srv/telegram-banhammer --appid 123456 --apphash 123abcdf --phone +123456 --password "pass_if_present" --channel_id 1234567 --ban_to 1666887600 --ban_search_duration 3m
```

#### Locally

```bash
go run ./main.go --appid 123456 --apphash 123abcdf --phone +123456 --password "pass_if_present" --channel_id 1234567 --ban_to 1666887600 --ban_search_duration 3m
```

### Clean messages, ban and kick users from the list

`ban_and_kick` must be set to the path to the file with the list of users to ban and kick.

#### Docker (recommended)

```bash
docker run --volume=./ban:/srv/ban paskal/telegram-banhammer:master /srv/telegram-banhammer --appid 123456 --apphash 123abcdf --phone +123456 --password "pass_if_present" --ban_and_kick ban/telegram-banhammer-2022-10-28T22-03-40.users.csv
```

#### Locally

```bash
go run ./main.go --appid 123456 --apphash 123abcdf --phone +123456 --password "pass_if_present" --channel_id 1234567 --ban_and_kick ban/telegram-banhammer-2022-10-28T22-03-40.users.csv
```
