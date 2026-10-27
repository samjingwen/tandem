//! Match time.Time's retained RFC3339 JSON loader, including its fallback
//! parser: single-digit hours, comma fractions, and boundary offsets are
//! accepted, but lowercase T/Z are not. Keep the source spelling elsewhere.
use time::{Date, Duration, Month, OffsetDateTime, Time};

fn digits(text: &[u8]) -> Option<u32> {
    if text.is_empty() {
        return None;
    }
    text.iter().try_fold(0_u32, |value, byte| {
        byte.is_ascii_digit().then_some(())?;
        value.checked_mul(10)?.checked_add(u32::from(byte - b'0'))
    })
}

pub(crate) fn parse(source: &str) -> Option<OffsetDateTime> {
    let bytes = source.as_bytes();
    if bytes.get(4) != Some(&b'-') || bytes.get(7) != Some(&b'-') || bytes.get(10) != Some(&b'T') {
        return None;
    }
    let year = i32::try_from(digits(bytes.get(..4)?)?).ok()?;
    let month = Month::try_from(u8::try_from(digits(bytes.get(5..7)?)?).ok()?).ok()?;
    let day = u8::try_from(digits(bytes.get(8..10)?)?).ok()?;
    let date = Date::from_calendar_date(year, month, day).ok()?;
    let hour_end = 11 + bytes.get(11..)?.iter().position(|byte| *byte == b':')?;
    if !(12..=13).contains(&hour_end) {
        return None;
    }
    let hour = u8::try_from(digits(bytes.get(11..hour_end)?)?).ok()?;
    let minute = u8::try_from(digits(bytes.get(hour_end + 1..hour_end + 3)?)?).ok()?;
    if bytes.get(hour_end + 3) != Some(&b':') {
        return None;
    }
    let second = u8::try_from(digits(bytes.get(hour_end + 4..hour_end + 6)?)?).ok()?;
    let mut position = hour_end + 6;
    let mut nanosecond = 0;
    if bytes
        .get(position)
        .is_some_and(|byte| matches!(*byte, b'.' | b','))
    {
        position += 1;
        let start = position;
        while bytes.get(position).is_some_and(u8::is_ascii_digit) {
            position += 1;
        }
        if position == start {
            return None;
        }
        let end = position.min(start + 9);
        nanosecond =
            digits(bytes.get(start..end)?)? * 10_u32.pow(u32::try_from(9 - (end - start)).ok()?);
    }
    let time = Time::from_hms_nano(hour, minute, second, nanosecond).ok()?;
    let tail = bytes.get(position..)?;
    let offset = match tail {
        b"Z" => 0,
        [sign @ (b'+' | b'-'), h1, h2, b':', m1, m2] => {
            let hours = digits(&[*h1, *h2])?;
            let minutes = digits(&[*m1, *m2])?;
            if hours > 24 || minutes > 60 {
                return None;
            }
            let seconds = i64::from(hours * 3600 + minutes * 60);
            if *sign == b'-' { -seconds } else { seconds }
        }
        _ => return None,
    };
    date.with_time(time)
        .assume_utc()
        .checked_sub(Duration::seconds(offset))
}
