-- Prefer G.722 for both phones. The old phone sends G.722, so a host that
-- negotiated A-law first made Asterisk transcode every call down to 8 kHz;
-- with G.722 on both legs the wideband audio is relayed unchanged.
UPDATE ps_endpoints SET allow = 'g722,alaw,ulaw' WHERE allow = 'alaw,ulaw,g722';
UPDATE ps_endpoints SET allow = 'opus,g722,alaw,ulaw' WHERE allow = 'opus,alaw,ulaw,g722';
