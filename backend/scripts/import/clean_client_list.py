#!/usr/bin/env python3
"""Clean the sales team's weekly "Master Client List" workbook into an import file.

    python3 clean_client_list.py MASTER.xlsx [--sheet "2026 SEP WEEK 4"] [--out clients-import.json]

Reads one weekly tab (the last tab by default) and writes:
  * clients-import.json  – records ready for `go run ./cmd/admin import-clients`
  * clients-import-review.csv – one line per source row with what was done and
    anything a person should check (shared phone numbers, unmatched emails, ...)

Nothing in the workbook is changed. Requires: pip install openpyxl
"""
import argparse, csv, datetime as dt, json, re, sys
from pathlib import Path

import openpyxl

COLS = ["no", "company", "contact", "phone", "email", "position", "location", "type",
        "last_visit", "last_quote", "opp_value", "next_follow_up", "sales_exec", "status", "notes"]

# Rep section heading key -> login used as the client's responsible person.
# Edit these in the JSON before importing if the real addresses differ.
OWNERS = {
    "MUSHEKWA": ("Mushekwa", "mushekwa@doslimited.com"),
    "KHUMBO": ("Khumbo", "khumbo@doslimited.com"),
    "DONVAN": ("Donvan", "donvan@doslimited.com"),
    "IZEMBA": ("Izemba Kapita", "izemba@doslimited.com"),
}

SECTOR = {
    "ngo": "NGO", "faith-organization": "NGO (faith-based)",
    "government": "Government", "government organisation": "Government",
    "government regulatory body": "Government (regulatory)", "statutory body": "Statutory body",
    "parastatal": "Parastatal",
    "finance": "Banking & finance", "bank": "Banking & finance",
    "insurance": "Insurance", "audit": "Audit & consulting", "consulting": "Audit & consulting",
    "hr consultant": "Audit & consulting",
    "education": "Education", "school": "Education", "university": "Education",
    "research centre": "Research",
    "health": "Health",
    "energy": "Energy", "energy production": "Energy",
    "mining": "Mining", "mine": "Mining",
    "farming": "Agriculture", "farm": "Agriculture", "agricultural": "Agriculture",
    "food production": "Manufacturing", "milling plant": "Manufacturing",
    "clothing production": "Manufacturing", "industry": "Manufacturing",
    "automobile industry": "Automotive",
    "logistics": "Transport & logistics", "transport": "Transport & logistics",
    "telecoms": "Telecoms", "telecommunication": "Telecoms",
    "media": "Media", "broadcasting": "Media",
    "retail": "Retail", "real-estate": "Real estate", "construction": "Construction",
    "call center": "Business services", "private company": "Private company",
    "multi sectoral": "Multi-sector",
}

EMPTY = {"", "n/a", "na", "none", "nil", "-", "tbc"}
EMAIL_RE = re.compile(r"[A-Za-z0-9._%+'-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")
TITLE_RE = re.compile(r"^(mr|mrs|ms|miss|dr|prof)\.?\s+", re.I)


def blank(v):
    return v is None or (isinstance(v, str) and v.strip().lower() in EMPTY)


def text(v):
    return None if blank(v) else re.sub(r"\s+", " ", str(v)).strip()


def numbered(s):
    """'1. A   2. B' -> {1: 'A', 2: 'B'}; None if the cell isn't numbered."""
    parts = re.split(r"(?:^|\s)(\d)\.\s*", " " + s)
    if len(parts) < 3:
        return None
    out = {}
    for i in range(1, len(parts) - 1, 2):
        val = re.sub(r"\s+", " ", parts[i + 1]).strip()
        if val:
            out[int(parts[i])] = val
    return out or None


def split_list(s):
    return [p.strip() for p in re.split(r"\s*(?:/|;|,|&|\band\b)\s*", s) if p.strip()]


def phone(raw):
    d = re.sub(r"\D", "", raw)
    if len(d) == 12 and d.startswith("260"):
        d = d[3:]
    elif len(d) == 10 and d.startswith("0"):
        d = d[1:]
    if len(d) == 9 and d[0] in "79652":
        return f"+260 {d[:3]} {d[3:6]} {d[6:]}"
    return raw.strip() or None


def phones_of(chunk):
    out = []
    for p in re.split(r"\s*/\s*|\s{2,}", chunk):
        p = re.sub(r"[-–]\s*[A-Za-z. ]+$", "", p).strip()  # "2609... - MRS. SILVIA"
        if re.search(r"\d{6,}", p):
            n = phone(p)
            if n and n not in out:
                out.append(n)
    return out


def clean_name(s):
    s = re.sub(r"\(.*?\)", "", s)
    s = TITLE_RE.sub("", s.strip())
    s = re.sub(r"\s+", " ", s).strip(" .-")
    return tidy_caps(s)


def tidy_caps(s):
    """'PROCUREMENT OFFICER' -> 'Procurement Officer'; leaves mixed case and short acronyms alone."""
    if not s or not s.isupper():
        return s
    words = []
    for w in s.split(" "):
        bare = re.sub(r"[^A-Z]", "", w)
        words.append(w if (len(bare) <= 2 or w in {"NGO", "ICT"}) else w.capitalize())
    return " ".join(words).replace("I.t", "IT")


GENERIC = {"procurement", "info", "admin", "sales", "support", "itsupport", "itmanager", "pharmacy",
           "office", "accounts", "finance", "zambia", "contact", "enquiries", "hr", "it"}


def name_from_email(e):
    local = e.split("@")[0]
    if any(b in GENERIC for b in re.split(r"[._-]", local.lower())):
        return None
    bits = [b for b in re.split(r"[._]", local) if b.isalpha()]
    return " ".join(b.capitalize() for b in bits) if len(bits) >= 2 else None


def fmt_date(v):
    if blank(v):
        return None
    if isinstance(v, dt.datetime):
        return v.strftime("%d %b %Y")
    if isinstance(v, (int, float)) and 30000 < v < 60000:  # Excel serial that lost its format
        return (dt.datetime(1899, 12, 30) + dt.timedelta(days=int(v))).strftime("%d %b %Y")
    return text(v)


def money(v):
    if blank(v):
        return None, None
    if isinstance(v, (int, float)):
        return f"ZMW {v:,.2f}", None
    s = str(v).strip()
    digits = re.sub(r"[^\d.]", "", s)
    if re.fullmatch(r"\d+(\.\d{1,2})?", digits):
        return f"ZMW {float(digits):,.2f}", None
    return s, f"opportunity value '{s}' is not a valid amount"


def build_contacts(r, issues):
    names_raw, phones_raw, emails_raw, pos_raw = (text(r[k]) or "" for k in ("contact", "phone", "email", "position"))
    if isinstance(r["phone"], (int, float)):
        phones_raw = str(int(r["phone"]))

    num_names = numbered(names_raw)
    names = list(num_names.values()) if num_names else split_list(names_raw)
    keys = list(num_names.keys()) if num_names else list(range(1, len(names) + 1))
    names = [clean_name(n) for n in names]

    # phones: numbered cells align by number; otherwise by position, else all on the first contact
    num_ph = numbered(phones_raw)
    if num_ph:
        ph_by = {k: phones_of(v) for k, v in num_ph.items()}
    else:
        flat = phones_of(phones_raw)
        ph_by = {keys[i]: [p] for i, p in enumerate(flat)} if names and len(flat) == len(names) else {keys[0] if keys else 1: flat}

    num_pos = numbered(pos_raw)
    if num_pos:
        pos_by = num_pos
    else:
        parts = split_list(pos_raw) if pos_raw else []
        pos_by = {keys[i]: p for i, p in enumerate(parts)} if names and len(parts) == len(names) else {k: pos_raw for k in keys}

    emails = []
    for e in EMAIL_RE.findall(emails_raw):
        e = e.strip("'").lower()
        if e not in emails:
            emails.append(e)

    contacts = [{"key": k, "name": n, "position": tidy_caps(text(pos_by.get(k))), "phones": ph_by.get(k, []), "email": None}
                for k, n in zip(keys, names) if n]

    # match emails to people by name tokens, then by position when the counts agree
    unmatched = []
    for e in emails:
        local = re.sub(r"[^a-z]", " ", e.split("@")[0])
        best = None
        for c in contacts:
            toks = [t.lower()[:5] for t in re.findall(r"[A-Za-z]{3,}", c["name"])]
            if c["email"] is None and any(t in local for t in toks):
                best = c
                break
        if best:
            best["email"] = e
        else:
            unmatched.append(e)
    free = [c for c in contacts if c["email"] is None]
    if unmatched and len(unmatched) == len(free):
        for c, e in zip(free, unmatched):
            c["email"] = e
        unmatched = []
    for e in list(unmatched):  # an email we can name becomes its own contact
        n = name_from_email(e)
        if n:
            contacts.append({"key": None, "name": n, "position": None, "phones": [], "email": e})
            unmatched.remove(e)
            issues.append(f"contact '{n}' added from email only")
    if not contacts and (emails or phones_raw):
        contacts.append({"key": None, "name": "Main contact", "position": text(pos_raw), "phones": phones_of(phones_raw),
                         "email": unmatched.pop(0) if unmatched else None})
        issues.append("no contact name – added as 'Main contact'")
    if unmatched:
        issues.append("emails kept in client notes: " + ", ".join(unmatched))
    if contacts and not any(c["phones"] for c in contacts) and phones_raw:
        issues.append(f"phone not understood: {phones_raw}")

    out = []
    for i, c in enumerate(contacts):
        out.append({
            "name": c["name"],
            "position": c["position"],
            "email": c["email"],
            "phone": " / ".join(c["phones"]) or None,
            "is_primary": i == 0,
        })
    return out, unmatched


def parse_sheet(ws):
    section = None
    for i, row in enumerate(ws.iter_rows(values_only=True), start=1):
        row = list(row[:15]) + [None] * (15 - len(row[:15]))
        if row[0] == "No." or (row[1] and str(row[1]).strip().upper() == "COMPANY"):
            continue
        others = [v for j, v in enumerate(row) if j != 1 and v is not None]
        if row[1] and not others and "CLIENT" in str(row[1]).upper():
            section = re.sub(r"[^A-Z]", "", str(row[1]).upper().split("'")[0])
            continue
        if section is None or all(v is None for v in row):
            continue
        yield i, section, dict(zip(COLS, row))


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("workbook")
    ap.add_argument("--sheet", help="tab to import (default: last tab)")
    ap.add_argument("--out", default="clients-import.json")
    a = ap.parse_args()

    wb = openpyxl.load_workbook(a.workbook, data_only=True)
    ws = wb[a.sheet] if a.sheet else wb.worksheets[-1]

    clients, review = [], []
    for row_no, section, r in parse_sheet(ws):
        issues = []
        company = text(r["company"])
        owner = section if section in OWNERS else None
        if not company:
            review.append([row_no, section, "", "skipped", "no company name"])
            continue
        if not owner:
            issues.append(f"unknown rep section '{section}'")

        contacts, other_emails = build_contacts(r, issues)
        sector_raw = text(r["type"])
        sector = SECTOR.get((sector_raw or "").lower(), sector_raw)
        opp, opp_issue = money(r["opp_value"])
        if opp_issue:
            issues.append(opp_issue)

        lines = [f"Imported from Sales master client list – {ws.title}, row {row_no}."]
        for label, val in (("Last visit", fmt_date(r["last_visit"])),
                           ("Last quotation", fmt_date(r["last_quote"])),
                           ("Opportunity value", opp),
                           ("Next follow-up", fmt_date(r["next_follow_up"])),
                           ("Status", text(r["status"])),
                           ("Notes", text(r["notes"])),
                           ("Other emails", ", ".join(other_emails))):
            if val:
                lines.append(f"{label}: {val}")
        exec_name = text(r["sales_exec"])
        if exec_name and owner and exec_name.split()[0].upper() != owner:
            lines.append(f"Sales executive on sheet: {exec_name}")
            issues.append(f"sales exec '{exec_name}' differs from section {owner}")

        name = company.strip()
        clients.append({
            "row": row_no,
            "owner": owner,
            "client": {
                "legal_name": name,
                "sector": sector,
                "billing_address": text(r["location"]),
                "default_currency": "ZMW",
                "notes": "\n".join(lines),
                "contacts": contacts,
            },
        })
        review.append([row_no, owner or section, name, f"ready ({len(contacts)} contacts)", "; ".join(issues)])

    out = Path(a.out)
    payload = {
        "source": Path(a.workbook).name,
        "sheet": ws.title,
        "generated_at": dt.datetime.now().isoformat(timespec="seconds"),
        "owners": [{"key": k, "name": n, "email": e} for k, (n, e) in OWNERS.items()],
        "clients": clients,
    }
    out.write_text(json.dumps(payload, indent=2, ensure_ascii=False))
    rev = out.with_name(out.stem + "-review.csv")
    with rev.open("w", newline="") as f:
        w = csv.writer(f)
        w.writerow(["sheet_row", "rep", "company", "result", "check"])
        w.writerows(review)
    flagged = sum(1 for x in review if x[4])
    print(f"{ws.title}: {len(clients)} clients ready, {sum(1 for x in review if x[3]=='skipped')} rows skipped, "
          f"{flagged} rows to check\n  -> {out}\n  -> {rev}")


if __name__ == "__main__":
    sys.exit(main())
