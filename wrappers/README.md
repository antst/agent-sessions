# Product wrappers

This split-ready tree contains product-named launchers, resident wrappers,
plugins, tests, and packaging. Wrapper code imports the bus only through
`bus/sdk/go` or `bus/sdk/js`; it never imports `bus/internal`.
