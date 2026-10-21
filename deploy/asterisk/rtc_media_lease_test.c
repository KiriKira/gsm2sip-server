#include <assert.h>
#include "rtc_media_lease.h"

int main(void)
{
	const time_t now = 1800000000;
	assert(!gsm2sip_rtc_lease_valid(NULL, now));
	assert(!gsm2sip_rtc_lease_valid("", now));
	assert(!gsm2sip_rtc_lease_valid("webrtc", now));
	assert(!gsm2sip_rtc_lease_valid("1800000012suffix", now));
	assert(!gsm2sip_rtc_lease_valid("999999999999999999999999999999999", now));
	assert(!gsm2sip_rtc_lease_valid("1800000000", now));
	assert(!gsm2sip_rtc_lease_valid("1799999999", now));
	assert(!gsm2sip_rtc_lease_valid("1800000021", now));
	assert(gsm2sip_rtc_lease_valid("1800000012", now));
	assert(gsm2sip_rtc_lease_valid("1800000012", now + 11));
	assert(!gsm2sip_rtc_lease_valid("1800000012", now + 12));
	return 0;
}
