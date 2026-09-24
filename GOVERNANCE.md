# Governance

How Gopherdex is run: who decides what, and how to get a say. The short version: anyone can contribute, people who keep showing up get more responsibility, and decisions happen in the open.

## Roles

| Role | What they do | How you get it |
|---|---|---|
| **Contributor** | Opens issues and pull requests, reviews, answers questions, improves the docs. | Take part. |
| **Triager** | Labels and reproduces issues, answers questions, points newcomers at `good first issue`s, closes duplicates. Has the *triage* permission on the repository, through the `@codebled/gopherdex-triage` team. | Ask in an issue after a few weeks of helpful comments and reviews. A maintainer adds you. |
| **Maintainer** | Reviews and merges pull requests, owns areas in [CODEOWNERS](.github/CODEOWNERS), cuts releases, handles security reports. Member of `@codebled/gopherdex-maintainers`. | Nominated by a maintainer, see below. |
| **Admin** | Repository settings, secrets, teams, the branch ruleset. | The organization owners. Kept to as few people as possible. |

The current maintainers and triagers are listed in [MAINTAINERS.md](MAINTAINERS.md).

## Becoming a maintainer

Maintainers look for:

- several merged pull requests over at least a few months, in more than one part of the code, that needed little rework;
- reviews of other people's pull requests that caught real problems and were kind about it;
- a feel for the parts that can't change lightly: immutable versions, the migration rules, the security-sensitive packages (accounts, sessions, tokens, uploads, permissions);
- reliability: replies to reviews and issues, even when the reply is "not this week".

A maintainer opens an issue titled *Nominate @handle as maintainer* with the evidence. The other maintainers have one week to object. With no objection, an admin adds the person to the maintainers team, they add themselves to MAINTAINERS.md in a pull request, and CODEOWNERS gets their areas.

## Decisions

- **Most decisions** are made in the issue or pull request where they come up, by the maintainers of that area. Silence for a week is agreement.
- **Bigger changes** get an issue labelled `proposal` first: the database schema, the JSON API or proxy routes, how publishing works, dependencies, licensing, and this document. The proposal says what changes, why, and what it breaks. It's open for at least a week before anyone starts on the code.
- **Disagreement** is talked through first. If that fails, the maintainers vote and a simple majority decides; the lead maintainer breaks ties.
- **Security fixes** are the exception: the maintainers on a report act first and explain afterwards in the advisory. See [SECURITY.md](SECURITY.md).

## Stepping down and inactivity

Life happens. A maintainer who wants a break says so and moves to the emeritus list in MAINTAINERS.md; they can come back by asking. A maintainer with no activity for six months is asked whether they want to stay; no answer within a month means emeritus. Emeritus maintainers keep our thanks and lose write access.

## Removal

The other maintainers can remove a maintainer who breaks the [Code of Conduct](CODE_OF_CONDUCT.md) or acts against the project's interests, by majority, and immediately in serious cases.

## Changing this document

By pull request, following the proposal process above.
