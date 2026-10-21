/* Bounded, trusted ARI lease for a SIP channel whose speech uses WebRTC.
 * Copyright 2026 gsm2sip contributors. SPDX-License-Identifier: GPL-2.0-only
 */
#ifndef GSM2SIP_RTC_MEDIA_LEASE_H
#define GSM2SIP_RTC_MEDIA_LEASE_H
#include <errno.h>
#include <stdlib.h>
#include <time.h>

static int gsm2sip_rtc_lease_valid(const char *value, time_t now)
{
	char *end;
	long long until;
	if (!value || !*value) {
		return 0;
	}
	errno = 0;
	until = strtoll(value, &end, 10);
	return !errno && end != value && !*end && until > now && until - now <= 20;
}
#endif
